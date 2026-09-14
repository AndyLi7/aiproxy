package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTraceNoncePersistsSingleClaimAcrossStoreInstances(t *testing.T) {
	db := openTraceSQLite(t)
	first := NewTraceStore(db)
	require.NoError(t, first.Migrate(context.Background()))
	expires := time.Now().UTC().Add(time.Minute)
	accepted, err := first.ClaimTraceNonce(context.Background(), strings.Repeat("a", 64), expires)
	require.NoError(t, err)
	require.True(t, accepted)
	second := NewTraceStore(db)
	accepted, err = second.ClaimTraceNonce(context.Background(), strings.Repeat("a", 64), expires)
	require.NoError(t, err)
	require.False(t, accepted)
}

func TestTraceNonceCleanupLeavesLiveClaims(t *testing.T) {
	db := openTraceSQLite(t)
	store := NewTraceStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	now := time.Now().UTC()
	_, err := store.ClaimTraceNonce(context.Background(), strings.Repeat("a", 64), now.Add(time.Second))
	require.NoError(t, err)
	_, err = store.ClaimTraceNonce(context.Background(), strings.Repeat("b", 64), now.Add(time.Minute))
	require.NoError(t, err)
	count, err := store.CleanTraceNonces(context.Background(), now.Add(2*time.Second), 500)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	accepted, err := store.ClaimTraceNonce(context.Background(), strings.Repeat("b", 64), now.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, accepted)
}
