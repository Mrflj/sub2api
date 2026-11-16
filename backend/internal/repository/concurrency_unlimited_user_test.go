package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func unlimitedUserCache(t *testing.T) (*concurrencyCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewConcurrencyCache(client, 15, 900).(*concurrencyCache), server
}

func TestUnlimitedUserSlotsCountAcrossInstancesAndRelease(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			cache, server := unlimitedUserCache(t)
			otherClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = otherClient.Close() })
			services := []*service.ConcurrencyService{
				service.NewConcurrencyService(cache),
				service.NewConcurrencyService(NewConcurrencyCache(otherClient, 15, 900)),
			}
			const requests = 24
			results := make([]*service.AcquireResult, requests)
			errs := make([]error, requests)
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = services[i%len(services)].AcquireUserSlot(context.Background(), 42, limit)
				}(i)
			}
			wg.Wait()
			for i, result := range results {
				require.NoError(t, errs[i])
				require.True(t, result.Acquired)
				t.Cleanup(result.ReleaseFunc)
			}
			ctx := context.Background()
			count, err := cache.GetUserConcurrency(ctx, 42)
			require.NoError(t, err)
			require.Equal(t, requests, count)
			load, err := cache.GetUsersLoadBatch(ctx, []service.UserWithConcurrency{{ID: 42, MaxConcurrency: limit}})
			require.NoError(t, err)
			require.Equal(t, requests, load[42].CurrentConcurrency)
			require.Zero(t, load[42].WaitingCount)
			require.Zero(t, load[42].LoadRate)
			require.True(t, server.Exists(userActiveIndexKey))
			// Replays refresh one member instead of inflating the count.
			ok, err := cache.AcquireUserSlot(ctx, 42, limit, results[0].RequestID)
			require.NoError(t, err)
			require.True(t, ok)
			count, err = cache.GetUserConcurrency(ctx, 42)
			require.NoError(t, err)
			require.Equal(t, requests, count)
			// Restoring a finite limit must see the already-active requests.
			ok, err = cache.AcquireUserSlot(ctx, 42, 1, "limited-request")
			require.NoError(t, err)
			require.False(t, ok)
			for _, result := range results {
				result.ReleaseFunc()
				result.ReleaseFunc()
			}
			count, err = cache.GetUserConcurrency(ctx, 42)
			require.NoError(t, err)
			require.Zero(t, count)
			require.False(t, server.Exists(userActiveIndexKey))
			ok, err = cache.AcquireUserSlot(ctx, 42, 1, "limited-request")
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, cache.ReleaseUserSlot(ctx, 42, "limited-request"))
		})
	}
}

func TestUnlimitedUserSlotsExpireWithoutChangingAccountAdmission(t *testing.T) {
	cache, server := unlimitedUserCache(t)
	ctx := context.Background()
	ok, err := cache.AcquireUserSlot(ctx, 42, 0, "crashed-request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.AcquireAccountSlot(ctx, 42, 0, "zero-capacity-account")
	require.NoError(t, err)
	require.False(t, ok)
	server.FastForward(16 * time.Minute)
	count, err := cache.GetUserConcurrency(ctx, 42)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestUnlimitedUserLiveTransferDoesNotDoubleCount(t *testing.T) {
	cache, _ := unlimitedUserCache(t)
	ctx := context.Background()
	user, err := service.NewConcurrencyService(cache).AcquireUserSlot(ctx, 20, 0)
	require.NoError(t, err)
	t.Cleanup(user.ReleaseFunc)
	ok, err := cache.AcquireAccountSlot(ctx, 10, 1, "account")
	require.NoError(t, err)
	require.True(t, ok)
	r := service.LiveLeaseTransferRequest{AccountID: 10, AccountMax: 1, AccountRequestID: "account", UserID: 20, UserRequestID: user.RequestID, APIKeyID: 30, LeaseID: "unlimited-live"}
	ok, err = cache.AcquireLiveLeaseTransferring(ctx, r)
	require.NoError(t, err)
	require.True(t, ok)
	count, err := cache.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	user.ReleaseFunc()
	count, err = cache.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, count, "late regular cleanup must not release the Live member")
	require.NoError(t, cache.ReleaseLiveLease(ctx, 10, 20, 30, r.LeaseID))
	count, err = cache.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestRegionalUnlimitedUserTracksWithoutAdmissionBudget(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			base, _ := unlimitedUserCache(t)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			var primary *slots.Primary
			var region *slots.Local
			if local {
				region = &slots.Local{DB: db, Region: "test-region"}
			} else {
				primary = &slots.Primary{DB: db, Limit: func(context.Context, *sql.Tx, slots.Resource) (int, error) {
					t.Fatal("unlimited statistics must not acquire an admission budget")
					return 0, nil
				}}
			}
			wrapped, err := NewRegionalConcurrencyCache(base, primary, region)
			require.NoError(t, err)
			svc := service.NewConcurrencyService(wrapped)
			result, err := svc.AcquireUserSlot(context.Background(), 42, 0)
			require.NoError(t, err)
			require.True(t, result.Acquired)
			t.Cleanup(result.ReleaseFunc)
			// SQL holds limited slots; Redis holds unlimited activity.
			mock.ExpectQuery("SELECT .* FROM regional_.*").WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(2))
			count, err := wrapped.GetUserConcurrency(context.Background(), 42)
			require.NoError(t, err)
			require.Equal(t, 3, count)
			mock.ExpectQuery("SELECT .* FROM regional_.*").WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(2))
			loads, err := wrapped.GetUsersLoadBatch(context.Background(), []service.UserWithConcurrency{{ID: 42}})
			require.NoError(t, err)
			require.Equal(t, 3, loads[42].CurrentConcurrency)
			require.Zero(t, loads[42].LoadRate)
			result.ReleaseFunc()
			count, err = base.GetUserConcurrency(context.Background(), 42)
			require.NoError(t, err)
			require.Zero(t, count)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
