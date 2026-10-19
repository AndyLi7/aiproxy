//nolint:testpackage // White-box coverage needs the package-private newSealosPostGroupConsumer fixture or implementation.
package balance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestSealosClaimLossStopsActualActions(t *testing.T) {
	var calls atomic.Int32

	lost := errors.New("fixture claim lost")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx := actionguard.With(context.Background(), func(context.Context) error {
		if calls.Load() > 0 {
			return lost
		}
		return nil
	})
	backend := &Sealos{accountURL: server.URL}
	_, _, err := backend.GetGroupRemainBalance(ctx, model.GroupCache{ID: "claim-action-fixture"})
	require.ErrorIs(t, err, lost)
	require.Equal(t, int32(1), calls.Load())

	_, err = backend.fetchRealNameFromAPI(ctx, "fixture-user")
	require.ErrorIs(t, err, lost)

	consumer := newSealosPostGroupConsumer(server.URL, "claim-action-fixture", "fixture-user")
	_, err = consumer.PostGroupConsume(ctx, "fixture-token", .3)
	require.ErrorIs(t, err, lost)
	require.Equal(t, int32(1), calls.Load())
}
