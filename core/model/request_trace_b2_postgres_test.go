package model

import (
	"context"
	"github.com/stretchr/testify/require"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTraceB2PostgresNonceSingleWinner(t *testing.T) {
	db := openTracePostgres(t)
	require.NoError(t, NewTraceStore(db).Migrate(context.Background()))
	var accepted atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := NewTraceStore(db).ClaimTraceNonce(context.Background(), strings.Repeat("a", 64), time.Now().Add(time.Minute))
			errors <- err
			if ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, accepted.Load())
}
