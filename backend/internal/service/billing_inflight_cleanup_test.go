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

type inflightCleanupCache struct {
	*rejectionInflightCache
	reserveCalls      atomic.Int32
	releaseCalls      atomic.Int32
	scopeReleaseCalls atomic.Int32
	reserveErrorAt    int32
	releaseFailures   int32
	releaseAfterWrite bool
	scopeReserveError bool
	scopeMu           sync.Mutex
	scopeHolds        map[string]map[string]float64
}

func newInflightCleanupCache() *inflightCleanupCache {
	return &inflightCleanupCache{
		rejectionInflightCache: &rejectionInflightCache{memInflightCache: newMemInflightCache(10)},
		scopeHolds:             make(map[string]map[string]float64),
	}
}

func (c *inflightCleanupCache) ReserveInflightBalance(ctx context.Context, userID int64, id string, amount, balance float64, ttl time.Duration) (bool, float64, error) {
	attempt := c.reserveCalls.Add(1)
	allowed, pending, err := c.memInflightCache.ReserveInflightBalance(ctx, userID, id, amount, balance, ttl)
	if allowed && attempt == c.reserveErrorAt {
		return false, 0, context.DeadlineExceeded // Redis committed; its reply was lost.
	}
	return allowed, pending, err
}

func (c *inflightCleanupCache) ReleaseInflightBalance(ctx context.Context, userID int64, id string) error {
	attempt := c.releaseCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if attempt <= c.releaseFailures {
		if c.releaseAfterWrite {
			_ = c.memInflightCache.ReleaseInflightBalance(ctx, userID, id)
		}
		return errors.New("injected Redis connection reset")
	}
	return c.memInflightCache.ReleaseInflightBalance(ctx, userID, id)
}

func (c *inflightCleanupCache) ReserveInflightScope(_ context.Context, scope, id string, amount float64, _ time.Duration) (float64, error) {
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if c.scopeHolds[scope] == nil {
		c.scopeHolds[scope] = make(map[string]float64)
	}
	var pending float64
	for _, amount := range c.scopeHolds[scope] {
		pending += amount
	}
	c.scopeHolds[scope][id] = amount
	if c.scopeReserveError {
		return 0, context.DeadlineExceeded
	}
	return pending, nil
}

func (c *inflightCleanupCache) ReleaseInflightScope(ctx context.Context, scope, id string) error {
	attempt := c.scopeReleaseCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if attempt <= c.releaseFailures {
		return errors.New("injected Redis connection reset")
	}
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	delete(c.scopeHolds[scope], id)
	return nil
}

func TestInflightReservation_ReserveReplyLostCleansOwnHold(t *testing.T) {
	for _, recheck := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_admission", true: "database_recheck"}[recheck], func(t *testing.T) {
			cache := newInflightCleanupCache()
			svc := newInflightSvc(t, cache, 60)
			user := &User{ID: 71, Status: StatusActive, Balance: 10}
			if recheck {
				cache.balance = 7
				svc.userRepo = &rejectionBalanceUserRepo{balance: 20}
			}
			held, err := svc.ReserveInflight(context.Background(), user, nil, nil, 1)
			require.NoError(t, err)
			t.Cleanup(held.Release)
			cache.reserveErrorAt = 2
			if recheck {
				cache.reserveErrorAt = 3
			}
			res, err := svc.ReserveInflight(context.Background(), user, nil, nil, 7)
			if recheck {
				require.ErrorIs(t, err, ErrBillingServiceUnavailable)
			} else {
				require.NoError(t, err, "preserve the initial fail-open policy")
			}
			require.Nil(t, res)
			require.Equal(t, 1, cache.count(), "only this attempt's uncertain hold must be removed")
			require.EqualValues(t, 1, held.refs.Load(), "another request retains ownership")
			held.HandlerDone()
			next, err := svc.ReserveInflight(context.Background(), user, nil, nil, 7)
			require.NoError(t, err, "a failed admission must not block the next request")
			next.HandlerDone()
			require.Zero(t, cache.count())
			require.Zero(t, cache.deducts.Load(), "reservations never debit the balance")
		})
	}
}

func TestInflightReservation_ReleaseRecoversTransientCacheFailure(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_delete", true: "after_delete"}[lostReply], func(t *testing.T) {
			cache := newInflightCleanupCache()
			cache.releaseFailures = 2
			cache.releaseAfterWrite = lostReply
			svc := newInflightSvc(t, cache, 60)
			ctx, cancel := context.WithCancel(context.Background())
			res, err := svc.ReserveInflight(ctx, &User{ID: 71}, nil, nil, 7)
			require.NoError(t, err)
			cancel()
			res.HandlerDone()
			require.Zero(t, cache.count(), "release must recover from transient Redis failures")
			require.EqualValues(t, 3, cache.releaseCalls.Load())
			res.HandlerDone()
			res.Release()
			require.EqualValues(t, 3, cache.releaseCalls.Load(), "completion remains idempotent")
			require.Zero(t, cache.deducts.Load())
		})
	}
}

func TestInflightReservation_ScopeReplyLostCleansOwnHold(t *testing.T) {
	cache := newInflightCleanupCache()
	svc := newInflightSvc(t, cache, 60)
	res, _ := svc.reserveInflightScope(context.Background(), nil, 71, "apikey:9", 1)
	t.Cleanup(res.Release)
	cache.scopeReserveError = true
	other, pending := svc.reserveInflightScope(context.Background(), nil, 71, "apikey:9", 7)
	require.Zero(t, pending, "preserve scope fail-open behavior")
	other.HandlerDone()
	require.Len(t, cache.scopeHolds["apikey:9"], 1)
	require.Contains(t, cache.scopeHolds["apikey:9"], res.requestID)
	res.HandlerDone()
	require.Empty(t, cache.scopeHolds["apikey:9"])
}

func TestInflightReservation_ReleaseFailureIsBoundedAndDoesNotSkipScopes(t *testing.T) {
	cache := newInflightCleanupCache()
	svc := newInflightSvc(t, cache, 60)
	res, err := svc.ReserveInflight(context.Background(), &User{ID: 71}, nil, nil, 7)
	require.NoError(t, err)
	res, _ = svc.reserveInflightScope(context.Background(), res, 71, "apikey:9", 7)
	cache.releaseFailures = 100
	res.HandlerDone()
	require.EqualValues(t, 3, cache.releaseCalls.Load())
	require.EqualValues(t, 3, cache.scopeReleaseCalls.Load(), "balance release failure must not skip scope cleanup")
	require.Zero(t, res.refs.Load())
	select {
	case <-res.renewDone:
	default:
		t.Fatal("failed cleanup must not keep renewing the hold")
	}
	res.Release()
	require.EqualValues(t, 3, cache.releaseCalls.Load(), "no unbounded cleanup loop")
}

func TestInflightReservation_CleanupRetriesWithFreshDeadline(t *testing.T) {
	attempts := 0
	releaseInflightReservation("user:71", "synthetic-reservation", func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		require.NoError(t, ctx.Err(), "the timed-out attempt's context must not be reused")
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(inflightReservationReleaseTimeout), deadline, time.Second)
		return nil
	})
	require.Equal(t, 2, attempts)
}

func TestInflightReservation_ConcurrentCompletionReleasesOnce(t *testing.T) {
	cache := newInflightCleanupCache()
	svc := newInflightSvc(t, cache, 60)
	res, err := svc.ReserveInflight(context.Background(), &User{ID: 71}, nil, nil, 7)
	require.NoError(t, err)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		done := res.Acquire()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			done()
			done()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		res.HandlerDone()
		res.HandlerDone()
	}()
	close(start)
	wg.Wait()
	require.Zero(t, cache.count())
	require.EqualValues(t, 1, cache.releaseCalls.Load())
	require.Zero(t, res.refs.Load())
}

func TestInflightReservation_SettlementChargesActualCostBeforeRelease(t *testing.T) {
	for _, releaseFailures := range []int32{0, 2} {
		t.Run(map[int32]string{0: "normal_release", 2: "release_retried"}[releaseFailures], func(t *testing.T) {
			cache := newInflightCleanupCache()
			cache.releaseFailures = releaseFailures
			svc := newInflightSvc(t, cache, 60)
			user := &User{ID: 71, Balance: 10}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res, err := svc.ReserveInflight(ctx, user, nil, nil, 7)
			require.NoError(t, err)
			t.Cleanup(res.Release)
			finishBilling := res.Acquire()
			t.Cleanup(finishBilling)
			res.HandlerDone()
			cancel()
			require.Equal(t, 1, cache.count(), "pending billing retains its reservation after the handler returns")
			balance, err := cache.GetUserBalance(context.Background(), user.ID)
			require.NoError(t, err)
			require.Equal(t, 10.0, balance, "reserving seven dollars does not debit the balance")

			balanceAfterCharge := 9.75
			repo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true, NewBalance: &balanceAfterCharge}}
			params := &postUsageBillingParams{
				Cost: &CostBreakdown{ActualCost: 0.25, TotalCost: 0.25}, User: user,
				APIKey: &APIKey{ID: 9}, Account: &Account{ID: 3, Type: AccountTypeAPIKey}, AccountRateMultiplier: 1,
			}
			usage := &UsageLog{RequestID: "settlement-before-release", UserID: user.ID, APIKeyID: 9, AccountID: 3, ActualCost: 0.25}
			deps := &billingDeps{billingCacheService: svc, deferredService: &DeferredService{}}
			applied, err := applyUsageBilling(ctx, usage.RequestID, usage, params, deps, repo)
			require.NoError(t, err)
			require.True(t, applied)
			require.NoError(t, repo.lastCtxErr, "client cancellation must not cancel an already observed bill")
			require.Equal(t, 0.25, repo.lastCmd.BalanceCost, "the repository receives actual cost, not the reservation estimate")
			require.EqualValues(t, 1, cache.deducts.Load())
			require.Equal(t, 1, cache.count(), "the billing task still owns the hold until cache synchronization finishes")
			finishBilling()
			require.Zero(t, cache.count())
			balance, err = cache.GetUserBalance(context.Background(), user.ID)
			require.NoError(t, err)
			require.Equal(t, balanceAfterCharge, balance)

			repo.result = &UsageBillingApplyResult{Applied: false}
			applied, err = applyUsageBilling(ctx, usage.RequestID, usage, params, deps, repo)
			require.NoError(t, err)
			require.False(t, applied, "a deduplicated billing replay cannot debit the cache twice")
			res.HandlerDone()
			finishBilling()
			balance, err = cache.GetUserBalance(context.Background(), user.ID)
			require.NoError(t, err)
			require.Equal(t, balanceAfterCharge, balance)
			require.EqualValues(t, 1, cache.deducts.Load(), "release retries and duplicate completion never repeat a charge")
			t.Logf("initial_balance=10 reservation=7 actual_charge=0.25 final_balance=%.2f active_reservations=%d cache_debits=%d", balance, cache.count(), cache.deducts.Load())
		})
	}
}
