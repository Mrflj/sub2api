//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpsUserConcurrencyIncludesUnlimitedActiveUsers(t *testing.T) {
	cache := &stubConcurrencyCacheForTest{usersLoadBatch: map[int64]*UserLoadInfo{
		1: {UserID: 1, CurrentConcurrency: 3},
		2: {UserID: 2},
		3: {UserID: 3, CurrentConcurrency: 1},
	}}
	svc := &OpsService{
		userRepo: &userRepoStubForListUsers{users: []User{
			{ID: 1, Email: "active@activation.local", Username: "active-user", Concurrency: 0},
			{ID: 2, Email: "idle@activation.local", Concurrency: 0},
			{ID: 3, Email: "limited@example.com", Concurrency: 2},
		}},
		concurrencyService: NewConcurrencyService(cache),
	}
	stats, collectedAt, err := svc.GetUserConcurrencyStats(context.Background())
	require.NoError(t, err)
	require.NotNil(t, collectedAt)
	require.Len(t, stats, 2)
	require.Equal(t, "active@activation.local", stats[1].UserEmail)
	require.Equal(t, "active-user", stats[1].Username)
	require.EqualValues(t, 3, stats[1].CurrentInUse)
	require.Zero(t, stats[1].MaxCapacity, "unlimited configuration must not be rewritten")
	require.Zero(t, stats[1].LoadPercentage)
	require.Zero(t, stats[1].WaitingInQueue)
	require.NotContains(t, stats, int64(2), "idle users should still be omitted")
	require.Equal(t, float64(50), stats[3].LoadPercentage)
}
