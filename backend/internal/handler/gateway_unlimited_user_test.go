package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUnlimitedUserHTTPCountsAndReleasesOnCancellation(t *testing.T) {
	helper, cache := newAPIKeyAdmissionHelper(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	c.Request = c.Request.WithContext(ctx)
	started := false
	first, err := helper.AcquireUserSlotWithWait(c, 42, 0, 77, 0, false, &started)
	require.NoError(t, err)
	defer first()
	second, acquired, err := helper.TryAcquireUserSlotForAPIKey(context.Background(), 42, 0, 78, 0)
	require.NoError(t, err)
	require.True(t, acquired)
	defer second()
	count, err := cache.GetUserConcurrency(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	loads, err := cache.GetUsersLoadBatch(context.Background(), []service.UserWithConcurrency{{ID: 42}})
	require.NoError(t, err)
	require.Zero(t, loads[42].WaitingCount)
	cancel()
	require.Eventually(t, func() bool {
		count, err := cache.GetUserConcurrency(context.Background(), 42)
		return err == nil && count == 1
	}, time.Second, time.Millisecond)
	first() // Deferred completion after cancellation cannot decrement another request.
	count, err = cache.GetUserConcurrency(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	second()
	count, err = cache.GetUserConcurrency(context.Background(), 42)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestUnlimitedUserWSTurnRetainsCountUntilOwnerReleases(t *testing.T) {
	helper, cache := newAPIKeyAdmissionHelper(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, acquired, err := helper.TryAcquireWSUserSlotForAPIKey(ctx, 42, 0, 77, 0)
	require.NoError(t, err)
	require.True(t, acquired)
	defer release()
	cancel()
	count, err := cache.GetUserConcurrency(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, 1, count, "a canceled WS turn still owns capacity while draining")
	release()
	release()
	count, err = cache.GetUserConcurrency(context.Background(), 42)
	require.NoError(t, err)
	require.Zero(t, count)
}
