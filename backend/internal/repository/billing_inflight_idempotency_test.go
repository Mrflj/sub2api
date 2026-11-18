package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInflightReservation_ReplayedReserveDoesNotRejectItsOwnHold(t *testing.T) {
	_, cache, _ := newInflightTestEnv(t, true, 60)
	ctx := context.Background()
	for replay := 0; replay < 3; replay++ {
		allowed, others, err := cache.ReserveInflightBalance(ctx, 71, "same-request", 7, 10, time.Minute)
		require.NoError(t, err)
		require.True(t, allowed, "a retry after a lost Redis reply must confirm the existing reservation")
		require.Zero(t, others)
		require.EqualValues(t, 1, inflightCount(t, cache, 71))
	}
	allowed, others, err := cache.ReserveInflightBalance(ctx, 71, "different-request", 7, 10, time.Minute)
	require.NoError(t, err)
	require.False(t, allowed, "a different request must still respect the concurrency guard")
	require.Equal(t, 7.0, others)
	require.NoError(t, cache.ReleaseInflightBalance(ctx, 71, "same-request"))
	require.Zero(t, inflightCount(t, cache, 71))
}

func TestInflightScopeReservation_ReplayedReserveOnlyCountsOthers(t *testing.T) {
	_, cache, _ := newInflightTestEnv(t, true, 60)
	ctx := context.Background()
	_, err := cache.ReserveInflightScope(ctx, "apikey:9", "other-request", 2, time.Minute)
	require.NoError(t, err)
	for replay := 0; replay < 3; replay++ {
		others, err := cache.ReserveInflightScope(ctx, "apikey:9", "same-request", 7, time.Minute)
		require.NoError(t, err)
		require.Equal(t, 2.0, others, "replayed Lua must not count the request's own quota twice")
	}
	_, hkey := billingInflightScopeKeys("apikey:9")
	count, err := cache.rdb.HLen(ctx, hkey).Result()
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
}
