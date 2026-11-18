//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type inflightWaitTestCache struct {
	*memInflightCache
	attempts         atomic.Int32
	afterReserve     func(bool)
	afterBalanceRead func()
	quota            *UserPlatformQuotaCacheEntry
}

func (c *inflightWaitTestCache) GetUserBalance(ctx context.Context, userID int64) (float64, error) {
	balance, err := c.memInflightCache.GetUserBalance(ctx, userID)
	if c.afterBalanceRead != nil {
		c.afterBalanceRead()
	}
	return balance, err
}

func (c *inflightWaitTestCache) ReserveInflightBalance(ctx context.Context, userID int64, id string, amount, balance float64, ttl time.Duration) (bool, float64, error) {
	c.attempts.Add(1)
	allowed, pending, err := c.memInflightCache.ReserveInflightBalance(ctx, userID, id, amount, balance, ttl)
	if c.afterReserve != nil {
		c.afterReserve(allowed)
	}
	return allowed, pending, err
}

func (c *inflightWaitTestCache) GetUserPlatformQuotaCache(context.Context, int64, string) (*UserPlatformQuotaCacheEntry, bool, error) {
	return c.quota, c.quota != nil, nil
}

func newInflightWaitTestService(t *testing.T) (*BillingCacheService, *inflightWaitTestCache, *APIKey) {
	t.Helper()
	cache := &inflightWaitTestCache{memInflightCache: newMemInflightCache(10)}
	svc := newInflightSvc(t, cache, 60)
	svc.cfg.Billing.InflightReservation.WaitTimeoutSeconds = 1
	svc.cfg.Billing.InflightReservation.WaitMaxWaiting = 8
	return svc, cache, &APIKey{ID: 9, User: &User{ID: 71, Status: StatusActive, Balance: 10}}
}

type inflightWaitResult struct {
	res *InflightReservation
	err error
}

func startInflightWait(t *testing.T, ctx context.Context, svc *BillingCacheService, key *APIKey) <-chan inflightWaitResult {
	t.Helper()
	result := make(chan inflightWaitResult, 1)
	go func() {
		res, err := svc.ReserveInflightWithWait(ctx, key, nil, 6, "")
		result <- inflightWaitResult{res, err}
	}()
	require.Eventually(t, func() bool {
		svc.inflightWaiters.mu.Lock()
		defer svc.inflightWaiters.mu.Unlock()
		return svc.inflightWaiters.users[key.User.ID] == 1
	}, 500*time.Millisecond, time.Millisecond)
	return result
}

func finishInflightWait(t *testing.T, result <-chan inflightWaitResult) inflightWaitResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("reservation wait did not finish within its bound")
		return inflightWaitResult{}
	}
}

func assertInflightWaitersEmpty(t *testing.T, svc *BillingCacheService) {
	t.Helper()
	svc.inflightWaiters.mu.Lock()
	defer svc.inflightWaiters.mu.Unlock()
	require.Zero(t, svc.inflightWaiters.total)
	require.Empty(t, svc.inflightWaiters.users)
}

func TestInflightWait_ImmediateAdmissionDoesNotWaitOrRecheckLimits(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	// The initial auth/billing gate owns these checks; a successful fast path
	// must not load them again or change existing fail-open behavior.
	svc.apiKeyRateLimitLoader = &inflightWaitLimitLoader{err: errors.New("must not be read")}
	key.Quota = 1
	res, err := svc.ReserveInflightWithWait(context.Background(), key, nil, 6, "")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1, cache.attempts.Load())
	res.HandlerDone()
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWait_ReleaseResumesSameRequestWithoutDebitOrExtraRPM(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	rpm := &userRPMCacheStub{}
	svc.userRPMCache = rpm
	require.NoError(t, svc.CheckBillingEligibility(context.Background(), key.User, key, nil, nil, ""))
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := startInflightWait(t, ctx, svc, key)
	require.Equal(t, 1, cache.count(), "waiting requests do not register a hold")
	held.HandlerDone()
	got := finishInflightWait(t, result)
	require.NoError(t, got.err)
	require.NotNil(t, got.res)
	t.Cleanup(got.res.Release)
	require.EqualValues(t, 1, atomic.LoadInt32(&rpm.userCalls), "polling must not consume RPM")
	require.EqualValues(t, 0, cache.deducts.Load(), "admission never charges the balance")
	balance, err := cache.GetUserBalance(ctx, key.User.ID)
	require.NoError(t, err)
	require.Equal(t, 10.0, balance)
	taskDone := got.res.Acquire()
	got.res.HandlerDone()
	require.Equal(t, 1, cache.count(), "async billing still owns the reservation")
	taskDone()
	require.Zero(t, cache.count())
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWait_TopUpResumesWithoutReleasingExistingHold(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := startInflightWait(t, ctx, svc, key)
	cache.mu.Lock()
	cache.balance = 20
	cache.mu.Unlock()
	got := finishInflightWait(t, result)
	require.NoError(t, got.err)
	require.NotNil(t, got.res)
	got.res.HandlerDone()
	require.Equal(t, 1, cache.count(), "only the resumed request's hold is released")
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWait_ExhaustedBalanceCannotUseLoneRequestException(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := startInflightWait(t, ctx, svc, key)
	require.NoError(t, cache.DeductUserBalance(ctx, key.User.ID, 10))
	held.HandlerDone()
	got := finishInflightWait(t, result)
	require.ErrorIs(t, got.err, ErrInsufficientBalance)
	require.Nil(t, got.res)
	require.Zero(t, cache.count())
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWait_SpendBetweenBalanceCheckAndReservationDoesNotAdmit(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var spend sync.Once
	cache.afterBalanceRead = func() {
		if cache.attempts.Load() >= 2 {
			spend.Do(func() {
				_ = cache.DeductUserBalance(ctx, key.User.ID, 10)
				held.HandlerDone()
			})
		}
	}
	result := startInflightWait(t, ctx, svc, key)
	got := finishInflightWait(t, result)
	t.Cleanup(got.res.Release)
	require.ErrorIs(t, got.err, ErrInsufficientBalance)
	require.Nil(t, got.res)
	require.Zero(t, cache.count())
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWait_TimeoutOrCancelDoesNotLeakHoldsOrWaiters(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "parent_deadline"} {
		t.Run(mode, func(t *testing.T) {
			svc, cache, key := newInflightWaitTestService(t)
			held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
			require.NoError(t, err)
			t.Cleanup(held.Release)
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "parent_deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			t.Cleanup(cancel)
			result := startInflightWait(t, ctx, svc, key)
			if mode == "cancel" {
				cancel()
			}
			got := finishInflightWait(t, result)
			switch mode {
			case "timeout":
				require.ErrorIs(t, got.err, ErrInflightBalanceBusy)
			case "cancel":
				require.ErrorIs(t, got.err, context.Canceled)
			case "parent_deadline":
				require.ErrorIs(t, got.err, context.DeadlineExceeded)
			}
			require.Nil(t, got.res)
			require.Equal(t, 1, cache.count())
			assertInflightWaitersEmpty(t, svc)
		})
	}
}

func TestInflightWait_CancelAfterAtomicAdmissionReleasesNewHold(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cache.afterReserve = func(allowed bool) {
		if allowed {
			cancel()
		}
	}
	result := startInflightWait(t, ctx, svc, key)
	held.HandlerDone()
	got := finishInflightWait(t, result)
	require.ErrorIs(t, got.err, context.Canceled)
	require.Nil(t, got.res)
	require.Zero(t, cache.count())
	assertInflightWaitersEmpty(t, svc)
}

type inflightWaitLimitLoader struct {
	APIKeyRepository
	data *APIKeyRateLimitData
	err  error
}

func (l *inflightWaitLimitLoader) GetRateLimitData(context.Context, int64) (*APIKeyRateLimitData, error) {
	return l.data, l.err
}

func TestInflightWait_RechecksKeyLimitsAndReleasesRejectedHold(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		setup   func(*APIKey)
		data    *APIKeyRateLimitData
		loadErr error
		want    error
	}{
		{"total_quota", func(k *APIKey) { k.Quota = 10 }, &APIKeyRateLimitData{QuotaUsed: 10}, nil, ErrAPIKeyQuotaExhausted},
		{"window_5h", func(k *APIKey) { k.RateLimit5h = 10 }, &APIKeyRateLimitData{Usage5h: 10, Window5hStart: &now}, nil, ErrAPIKeyRateLimit5hExceeded},
		{"window_1d", func(k *APIKey) { k.RateLimit1d = 10 }, &APIKeyRateLimitData{Usage1d: 10, Window1dStart: &now}, nil, ErrAPIKeyRateLimit1dExceeded},
		{"window_7d", func(k *APIKey) { k.RateLimit7d = 10 }, &APIKeyRateLimitData{Usage7d: 10, Window7dStart: &now}, nil, ErrAPIKeyRateLimit7dExceeded},
		{"load_failure", func(k *APIKey) { k.Quota = 10 }, nil, errors.New("unavailable"), ErrBillingServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, cache, key := newInflightWaitTestService(t)
			tc.setup(key)
			svc.apiKeyRateLimitLoader = &inflightWaitLimitLoader{data: tc.data, err: tc.loadErr}
			held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
			require.NoError(t, err)
			t.Cleanup(held.Release)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			result := startInflightWait(t, ctx, svc, key)
			held.HandlerDone()
			got := finishInflightWait(t, result)
			require.ErrorIs(t, got.err, tc.want)
			require.Nil(t, got.res)
			require.Zero(t, cache.count())
			assertInflightWaitersEmpty(t, svc)
		})
	}
}

func TestInflightWait_DisabledAndFullQueueReturnBusyImmediately(t *testing.T) {
	for _, mode := range []string{"timeout_disabled", "queue_disabled", "queue_full"} {
		t.Run(mode, func(t *testing.T) {
			svc, cache, key := newInflightWaitTestService(t)
			held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
			require.NoError(t, err)
			t.Cleanup(held.Release)
			switch mode {
			case "timeout_disabled":
				svc.cfg.Billing.InflightReservation.WaitTimeoutSeconds = 0
			case "queue_disabled":
				svc.cfg.Billing.InflightReservation.WaitMaxWaiting = 0
			case "queue_full":
				svc.cfg.Billing.InflightReservation.WaitMaxWaiting = 1
				release, ok := svc.inflightWaiters.acquire(key.User.ID, 1)
				require.True(t, ok)
				defer release()
			}
			before := cache.attempts.Load()
			res, err := svc.ReserveInflightWithWait(context.Background(), key, nil, 6, "")
			require.ErrorIs(t, err, ErrInflightBalanceBusy)
			require.Nil(t, res)
			require.Equal(t, before+1, cache.attempts.Load(), "no polling when waiting is disabled or full")
			require.Equal(t, 1, cache.count())
		})
	}
}

func TestInflightWait_RechecksPlatformQuotaBeforeProceeding(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	limit := 1.0
	now := time.Now()
	cache.quota = &UserPlatformQuotaCacheEntry{
		SchemaVersion: UserPlatformQuotaCacheSchemaV1,
		DailyLimitUSD: &limit, DailyUsageUSD: 1, DailyWindowStart: &now,
		WeeklyWindowStart: &now, MonthlyWindowStart: &now,
	}
	svc.userPlatformQuotaRepo = &fakeQuotaRepo{}
	held, err := svc.ReserveInflight(context.Background(), key.User, nil, nil, 6)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	releaseTimer := time.AfterFunc(50*time.Millisecond, held.HandlerDone)
	defer releaseTimer.Stop()
	res, err := svc.ReserveInflightWithWait(context.Background(), key, nil, 6, PlatformAnthropic)
	require.ErrorIs(t, err, ErrUserPlatformDailyQuotaExhausted)
	require.Nil(t, res)
	require.Zero(t, cache.count())
	assertInflightWaitersEmpty(t, svc)
}

func TestInflightWaiterLimitsArePerUserBoundedAndIdempotent(t *testing.T) {
	w := &inflightReservationWaiters{}
	first, ok := w.acquire(1, 1)
	require.True(t, ok)
	_, ok = w.acquire(1, 1)
	require.False(t, ok, "one user's limit applies before the process limit is reached")
	other, ok := w.acquire(2, 1)
	require.True(t, ok, "another user has independent wait capacity")
	first()
	other()
	releases := make([]func(), 0, maxInflightReservationWaiters)
	for i := 0; i < maxInflightReservationWaiters; i++ {
		release, ok := w.acquire(int64(i+1), 1)
		require.True(t, ok)
		releases = append(releases, release)
	}
	_, ok = w.acquire(1, 1)
	require.False(t, ok)
	_, ok = w.acquire(999, 1)
	require.False(t, ok)
	var wg sync.WaitGroup
	for _, release := range releases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release()
			release()
		}()
	}
	wg.Wait()
	require.Zero(t, w.total)
	require.Empty(t, w.users)
	release, ok := w.acquire(1, 1)
	require.True(t, ok)
	release()
}
