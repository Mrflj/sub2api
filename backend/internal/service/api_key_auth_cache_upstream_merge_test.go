package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthCacheMergedContextLimitAndWebSearch(t *testing.T) {
	svc := &APIKeyService{}
	key := &APIKey{ID: 10, UserID: 20, User: &User{ID: 20}, GroupID: int64Ptr(7),
		AllowLongContext: true, ContextTokenLimit: 1234,
		Group: &Group{ID: 7, WebSearchAccountID: 88, ForceOpenAIFast: true, ForceOpenAIUltrafast: true}}
	snapshot := svc.snapshotFromAPIKey(context.Background(), key)
	payload, err := json.Marshal(APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var cached APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &cached))
	got, used, err := svc.applyAuthCacheEntry("sk-merged-cache-fixture", &cached)
	require.NoError(t, err)
	require.True(t, used)
	require.True(t, got.AllowLongContext)
	require.Equal(t, 1234, got.ContextTokenLimit)
	require.NotNil(t, got.Group)
	require.EqualValues(t, 88, got.Group.WebSearchAccountID)
	require.True(t, got.Group.ForceOpenAIFast)
	require.True(t, got.Group.ForceOpenAIUltrafast)
	// Each pre-merge snapshot misses fields from the other branch.
	for _, oldVersion := range []int{33, 34, 35, 36} {
		cached.Snapshot.Version = oldVersion
		_, used, err = svc.applyAuthCacheEntry("sk-merged-cache-fixture", &cached)
		require.NoError(t, err)
		require.False(t, used, "pre-merge snapshot v%d must be refreshed", oldVersion)
	}
}
