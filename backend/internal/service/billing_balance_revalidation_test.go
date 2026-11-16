//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type rejectionBalanceUserRepo struct {
	UserRepository
	balance float64
	err     error
	calls   atomic.Int64
}

func (r *rejectionBalanceUserRepo) GetByID(_ context.Context, id int64) (*User, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	return &User{ID: id, Status: StatusActive, Balance: r.balance}, nil
}

type rejectionInflightCache struct {
	*memInflightCache
	invalidations atomic.Int64
}

func (c *rejectionInflightCache) InvalidateUserBalance(context.Context, int64) error {
	c.invalidations.Add(1)
	return nil
}

func TestBalanceEligibility_RechecksStaleExhaustedCache(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: -0.01}
	users := &rejectionBalanceUserRepo{balance: 1.52}
	svc := NewBillingCacheService(cache, users, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)

	err := svc.CheckBillingEligibility(context.Background(), &User{ID: 71, Status: StatusActive, Balance: 1.52}, nil, nil, nil, "")
	require.NoError(t, err, "a stale Redis value must not reject a user whose authoritative balance is $1.52")
	require.EqualValues(t, 1, users.calls.Load())
	require.EqualValues(t, 1, cache.invalidateCalls.Load())
}

func TestBalanceEligibility_CoalescesConcurrentRejectionRechecks(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: -0.01}
	users := &balanceLoadUserRepoStub{balance: 1.52, delay: 80 * time.Millisecond}
	svc := NewBillingCacheService(cache, users, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)
	const n = 16
	start := make(chan struct{})
	errors := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errors <- svc.CheckBillingEligibility(context.Background(), &User{ID: 71, Status: StatusActive}, nil, nil, nil, "")
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, users.calls.Load(), "concurrent false rejections must share a bounded DB read")
}

func TestInflightReservation_RechecksStaleBalanceBeforeRejecting(t *testing.T) {
	cache := &rejectionInflightCache{memInflightCache: newMemInflightCache(0.25)}
	svc := newInflightSvc(t, cache, 60)
	users := &rejectionBalanceUserRepo{balance: 1.52}
	svc.userRepo = users
	user := &User{ID: 71, Status: StatusActive, Balance: 1.52}

	first, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.20)
	require.NoError(t, err)
	t.Cleanup(first.Release)
	require.Zero(t, users.calls.Load(), "the successful fast path must not add a DB read")

	second, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.50)
	require.NoError(t, err, "DB balance $1.52 covers both reservations; cached $0.25 does not")
	require.NotNil(t, second)
	t.Cleanup(second.Release)
	require.EqualValues(t, 1, users.calls.Load())
	require.EqualValues(t, 1, cache.invalidations.Load())
	require.Equal(t, 2, cache.count())
	require.Equal(t, 1.52, users.balance, "reservations never debit the database balance")
}

func TestInflightReservation_RecheckDatabaseFailureDoesNotMisreportBalance(t *testing.T) {
	cache := &rejectionInflightCache{memInflightCache: newMemInflightCache(1.52)}
	svc := newInflightSvc(t, cache, 60)
	svc.userRepo = &rejectionBalanceUserRepo{err: errors.New("balance database unavailable")}
	user := &User{ID: 71, Status: StatusActive}
	first, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	second, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrBillingServiceUnavailable)
	require.NotErrorIs(t, err, ErrInsufficientBalance)
	require.Equal(t, 1, cache.count(), "a failed verification must not create another hold")
}

func TestInflightReservation_PositiveBalanceBusyIsRetryableAndDoesNotDebit(t *testing.T) {
	cache := &rejectionInflightCache{memInflightCache: newMemInflightCache(1.52)}
	svc := newInflightSvc(t, cache, 60)
	users := &rejectionBalanceUserRepo{balance: 1.52}
	svc.userRepo = users
	user := &User{ID: 71, Status: StatusActive, Balance: 1.52}
	first, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	second, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrInflightBalanceBusy)
	require.NotErrorIs(t, err, ErrInsufficientBalance)
	require.NotContains(t, infraerrors.Message(err), "充值")
	metadata := infraerrors.FromError(err).Metadata
	require.Equal(t, "1.52", metadata["balance_usd"])
	require.Equal(t, "0.9", metadata["reserved_usd"])
	require.Equal(t, "0.9", metadata["estimated_usd"])
	require.EqualValues(t, 1, users.calls.Load())
	require.Zero(t, cache.invalidations.Load(), "a correct balance cache need not be discarded")

	first.HandlerDone()
	second, err = svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.NoError(t, err, "the same request must pass after the old hold is released, without a recharge")
	second.HandlerDone()
	require.Equal(t, 1.52, users.balance)
	require.Zero(t, cache.count())
}

func TestInflightReservation_AuthoritativeExhaustedBalanceStillRejected(t *testing.T) {
	cache := &rejectionInflightCache{memInflightCache: newMemInflightCache(1.52)}
	svc := newInflightSvc(t, cache, 60)
	svc.userRepo = &rejectionBalanceUserRepo{balance: 0}
	user := &User{ID: 71, Status: StatusActive}
	first, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	second, err := svc.ReserveInflight(context.Background(), user, nil, nil, 0.90)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrInsufficientBalance)
	require.NotErrorIs(t, err, ErrInflightBalanceBusy)
	require.Equal(t, 1, cache.count())
}

func TestBalanceEligibility_AuthoritativeFloorAndDatabaseFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		balance float64
		err     error
		want    error
	}{
		{"exhausted", 0, nil, ErrInsufficientBalance},
		{"below_configured_floor", 0.005, nil, ErrInsufficientBalance},
		{"at_configured_floor", 0.01, nil, nil},
		{"database_unavailable", 1.52, errors.New("database unavailable"), ErrBillingServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &balanceEligibilityCacheStub{balance: -1}
			users := &rejectionBalanceUserRepo{balance: tc.balance, err: tc.err}
			cfg := &config.Config{}
			cfg.Billing.MinimumBalanceReserve = 0.01
			svc := NewBillingCacheService(cache, users, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(svc.Stop)
			err := svc.CheckBillingEligibility(context.Background(), &User{ID: 71, Status: StatusActive, Balance: 1.52}, nil, nil, nil, "")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.EqualValues(t, 1, users.calls.Load())
		})
	}
}
