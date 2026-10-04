//nolint:testpackage
package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// Polling reads stored state only: an unsettled prepaid task never triggers a
// wallet call from a poll, and a polling loop is limited per API key.
func TestImageTaskPollReadsStoredStateOnly(t *testing.T) {
	walletCalls := 0
	wallet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		walletCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer wallet.Close()
	oldBalance := balance.Default
	balance.Default = balance.NewExternalHTTP(wallet.URL, "local-only")
	t.Cleanup(func() { balance.Default = oldBalance })

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "poll.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })
	require.NoError(t, db.Create(&model.ImageTask{
		ID: "poll-task", Model: "image", Status: "processing", GroupID: "poll-limit-group", TokenID: 1, Fingerprint: "f",
		PrepaymentQuoteJSON: `{"version":1}`, BillingOperationID: "image:poll-task",
	}).Error)

	poll := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/images/tasks/poll-task", nil)
		c.Params = gin.Params{{Key: "id", Value: "poll-task"}}
		c.Set(middleware.Group, model.GroupCache{ID: "poll-limit-group"})
		c.Set(middleware.Token, model.TokenCache{ID: 1})
		GetImageTask(c)
		return w
	}
	w := poll()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"processing"`)
	require.Zero(t, walletCalls)

	limited := false
	for range 100 {
		if w = poll(); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		require.Equal(t, http.StatusOK, w.Code)
	}
	require.True(t, limited, "a tight polling loop must be rate limited")
	require.Contains(t, w.Body.String(), "rate_limit_exceeded")
	require.Zero(t, walletCalls)
}
