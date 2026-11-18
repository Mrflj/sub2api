package service

import (
	"context"
	"errors"
	"sync"
	"time"
)

const maxInflightReservationWaiters = 256

type inflightReservationWaiters struct {
	mu    sync.Mutex
	users map[int64]int
	total int
}

func (w *inflightReservationWaiters) acquire(userID int64, limit int) (func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if limit <= 0 || w.total >= maxInflightReservationWaiters || w.users[userID] >= limit {
		return nil, false
	}
	if w.users == nil {
		w.users = make(map[int64]int)
	}
	w.users[userID]++
	w.total++
	return sync.OnceFunc(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.total--
		w.users[userID]--
		if w.users[userID] == 0 {
			delete(w.users, userID)
		}
	}), true
}

// ReserveInflightWithWait preserves the immediate reservation fast path. Only
// temporary contention waits; polling never forwards a request or increments RPM.
// Like ReserveInflight, it must follow the request's CheckBillingEligibility gate.
func (s *BillingCacheService) ReserveInflightWithWait(ctx context.Context, apiKey *APIKey, subscription *UserSubscription, estimate float64, platform string) (*InflightReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if apiKey == nil || apiKey.User == nil {
		return nil, nil
	}
	res, err := s.ReserveInflight(ctx, apiKey.User, apiKey.Group, subscription, estimate)
	if ctx.Err() != nil {
		res.Release()
		return nil, ctx.Err()
	}
	cfg, enabled := s.inflightReservationConfig()
	if !errors.Is(err, ErrInflightBalanceBusy) || !enabled || cfg.WaitTimeoutSeconds <= 0 {
		return res, err
	}
	releaseWait, acquired := s.inflightWaiters.acquire(apiKey.User.ID, cfg.WaitMaxWaiting)
	if !acquired {
		return nil, err
	}
	defer releaseWait()

	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.WaitTimeoutSeconds)*time.Second)
	defer cancel()
	lastBusy := err
	waitError := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return lastBusy
	}
	interval := 250 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, waitError()
		case <-timer.C:
		}
		if waitCtx.Err() != nil {
			return nil, waitError()
		}
		// A completed request may have spent the last balance before releasing
		// its reservation. Do not let the lone-request exception admit it again.
		if checkErr := s.checkBalanceEligibility(waitCtx, apiKey.User, apiKey); checkErr != nil {
			if waitCtx.Err() != nil {
				return nil, waitError()
			}
			return nil, checkErr
		}
		res, err = s.ReserveInflight(waitCtx, apiKey.User, apiKey.Group, subscription, estimate)
		if waitCtx.Err() != nil {
			res.Release()
			return nil, waitError()
		}
		if !errors.Is(err, ErrInflightBalanceBusy) {
			if err != nil {
				return nil, err
			}
			checkErr := s.checkInflightWaitLimits(waitCtx, apiKey, platform)
			if waitCtx.Err() != nil {
				res.Release()
				return nil, waitError()
			}
			if checkErr != nil {
				res.Release()
				return nil, checkErr
			}
			return res, nil
		}
		lastBusy = err
		if interval < 2*time.Second {
			interval *= 2
		}
		timer.Reset(interval)
	}
}

// Check monetary limits again after waiting, without CheckBillingEligibility's
// RPM increment. Key limits use fresh usage when a repository is available.
func (s *BillingCacheService) checkInflightWaitLimits(ctx context.Context, apiKey *APIKey, platform string) error {
	// Settlement can land between the polling check and atomic reservation.
	if err := s.checkBalanceEligibility(ctx, apiKey.User, apiKey); err != nil {
		return err
	}
	if err := s.checkUserPlatformQuotaEligibility(ctx, apiKey.User.ID, platform); err != nil {
		return err
	}
	if s.apiKeyRateLimitLoader != nil && (apiKey.Quota > 0 || apiKey.HasRateLimits()) {
		data, err := s.apiKeyRateLimitLoader.GetRateLimitData(ctx, apiKey.ID)
		if err != nil || data == nil {
			return ErrBillingServiceUnavailable.WithCause(err)
		}
		switch {
		case apiKey.Quota > 0 && data.QuotaUsed >= apiKey.Quota:
			return ErrAPIKeyQuotaExhausted
		case apiKey.RateLimit5h > 0 && data.EffectiveUsage5h() >= apiKey.RateLimit5h:
			return ErrAPIKeyRateLimit5hExceeded
		case apiKey.RateLimit1d > 0 && data.EffectiveUsage1d() >= apiKey.RateLimit1d:
			return ErrAPIKeyRateLimit1dExceeded
		case apiKey.RateLimit7d > 0 && data.EffectiveUsage7d() >= apiKey.RateLimit7d:
			return ErrAPIKeyRateLimit7dExceeded
		}
		return nil
	}
	if apiKey.Quota > 0 && apiKey.QuotaUsed >= apiKey.Quota {
		return ErrAPIKeyQuotaExhausted
	}
	if apiKey.HasRateLimits() {
		return s.checkAPIKeyRateLimits(ctx, apiKey)
	}
	return nil
}
