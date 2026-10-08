package imageprepayment

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// fakeWallet records prepayment actions, with settle outcomes spelled out.
type fakeWallet struct {
	mu      sync.Mutex
	actions []string
}

func (w *fakeWallet) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var c balance.PrepaymentCommand
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("decode wallet command: %v", err)
		}
		action := c.Action
		if c.Outcome != nil {
			action += ":" + c.Outcome.Kind + "/" + c.Outcome.Reason
		}
		w.mu.Lock()
		w.actions = append(w.actions, action)
		w.mu.Unlock()
		_, _ = rw.Write([]byte(`{"code":0,"data":{"id":"op","status":"refunded","prepaidMicros":600000,"chargedMicros":0,"refundMicros":600000}}`))
	}))
}

// A generation timeout refunds the hold in full as platform_failure in one
// wallet call. Provider failures keep the actual-cost path and other failures
// keep the plain refund.
func TestSyncRefundsGenerationTimeoutInOneCall(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
	oldDB, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	t.Cleanup(func() { model.LogDB, balance.Default = oldDB, oldBalance })

	for code, want := range map[string][]string{
		model.AsyncGenerationTimeoutCode: {"accept_attempt", "settle:failed/platform_failure", "get"},
		"upstream_failed":                {"accept_attempt", "execution_finished", "get"},
		"archive_size_exceeded":          {"accept_attempt", "settle:failed/platform_failure", "get"},
	} {
		t.Run(code, func(t *testing.T) {
			wallet := &fakeWallet{}
			server := wallet.serve(t)
			defer server.Close()
			balance.Default = balance.NewExternalHTTP(server.URL, "test")
			task := &model.ImageTask{
				ID: "task-" + code, GroupID: "g", TokenID: 1, Status: "failed", UpstreamID: "upstream",
				BillingOperationID: "op", PrepaymentQuoteJSON: `{"version":1}`,
				Attempts: []model.ImageTaskAttempt{{Failure: failover.Failure{Acceptance: failover.Accepted}}},
				Error:    &model.ImageTaskError{Code: code},
			}
			require.NoError(t, db.Create(task).Error)
			receipt, err := Sync(t.Context(), task)
			require.NoError(t, err)
			require.Equal(t, "refunded", receipt.Status)
			require.Equal(t, want, wallet.actions)
			for _, action := range wallet.actions {
				require.NotContains(t, action, model.AsyncGenerationTimeoutCode, "the wallet reason stays platform_failure")
			}
		})
	}
}
