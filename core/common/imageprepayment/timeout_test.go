package imageprepayment_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/failover"
	"github.com/labring/aiproxy/core/common/imageprepayment"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// fakeWallet records prepayment actions, with settle outcomes spelled out.
type fakeWallet struct {
	mu      sync.Mutex
	actions []string
}

func (w *fakeWallet) serve(t *testing.T) *httptest.Server {
	t.Helper()

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

		_, _ = rw.Write(
			[]byte(
				`{"code":0,"data":{"id":"op","status":"refunded","prepaidMicros":600000,"chargedMicros":0,"refundMicros":600000}}`,
			),
		)
	}))
}

// Every failed task is refunded in full as platform_failure in one wallet
// call, whatever its error code (owner rules 2026-10-02 and 2026-10-08):
// accepted provider failures included, and never after execution_finished.
func TestSyncRefundsGenerationTimeoutInOneCall(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	oldDB, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	t.Cleanup(func() { model.LogDB, balance.Default = oldDB, oldBalance })

	for code, want := range map[string][]string{
		model.AsyncGenerationTimeoutCode: {"accept_attempt", "settle:failed/platform_failure", "get"},
		"upstream_failed":                {"accept_attempt", "settle:failed/platform_failure", "get"},
		"invalid_parameters":             {"accept_attempt", "settle:failed/platform_failure", "get"},
		"archive_size_exceeded":          {"accept_attempt", "settle:failed/platform_failure", "get"},
	} {
		t.Run(code, func(t *testing.T) {
			wallet := &fakeWallet{}

			server := wallet.serve(t)
			defer server.Close()

			balance.Default = balance.NewExternalHTTP(server.URL, "test")
			task := &model.ImageTask{
				ID:                  "task-" + code,
				GroupID:             "g",
				TokenID:             1,
				Status:              "failed",
				UpstreamID:          "upstream",
				BillingOperationID:  "op",
				PrepaymentQuoteJSON: `{"version":1}`,
				Attempts: []model.ImageTaskAttempt{
					{Failure: failover.Failure{Acceptance: failover.Accepted}},
				},
				Error: &model.ImageTaskError{Code: code},
			}
			require.NoError(t, db.Create(task).Error)
			receipt, err := imageprepayment.Sync(t.Context(), task)
			require.NoError(t, err)
			require.Equal(t, "refunded", receipt.Status)
			require.Equal(t, want, wallet.actions)

			for _, action := range wallet.actions {
				require.NotContains(
					t,
					action,
					model.AsyncGenerationTimeoutCode,
					"the wallet reason stays platform_failure",
				)
			}
		})
	}
}

// A submission the provider never confirmed (no upstream ID, unknown attempt)
// that timed out is refunded in one call too: nothing is accepted, rejected or
// marked finished on its behalf.
func TestSyncRefundsUnconfirmedGenerationTimeoutInOneCall(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	oldDB, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	t.Cleanup(func() { model.LogDB, balance.Default = oldDB, oldBalance })

	wallet := &fakeWallet{}

	server := wallet.serve(t)
	defer server.Close()

	balance.Default = balance.NewExternalHTTP(server.URL, "test")
	task := &model.ImageTask{
		ID:                  "unconfirmed",
		GroupID:             "g",
		TokenID:             1,
		Status:              "failed",
		BillingOperationID:  "op",
		PrepaymentQuoteJSON: `{"version":1}`,
		Attempts: []model.ImageTaskAttempt{
			{Failure: failover.Failure{Acceptance: failover.Unknown}},
		},
		Error: &model.ImageTaskError{Code: model.AsyncGenerationTimeoutCode},
	}
	require.NoError(t, db.Create(task).Error)
	receipt, err := imageprepayment.Sync(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, "refunded", receipt.Status)
	require.Equal(t, []string{"settle:failed/platform_failure", "get"}, wallet.actions)
}

// R5: a failed task whose admission never reached the wallet used to re-send
// its settle every recovery pass forever. Its billing is closed only when the
// application itself answers "Prepayment not found", the task claimed no
// attempt and it failed more than an hour ago; any other 404 (a platform page
// during a deploy) keeps the task in recovery.
func TestSyncClosesBillingOnlyWhenTheWalletConfirmsNoHold(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	oldDB, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	t.Cleanup(func() { model.LogDB, balance.Default = oldDB, oldBalance })

	const appNotFound = `{"code":-1,"message":"Prepayment not found"}`
	for _, tc := range []struct {
		name, body string
		failedAgo  time.Duration
		attempts   []model.ImageTaskAttempt
		closed     bool
	}{
		{"application answer, failed long ago", appNotFound, 2 * time.Hour, nil, true},
		{"platform 404", `<html>Not Found</html>`, 2 * time.Hour, nil, false},
		{"failed recently", appNotFound, 30 * time.Minute, nil, false},
		{"an attempt was claimed", appNotFound, 2 * time.Hour, []model.ImageTaskAttempt{{Failure: failover.Failure{Acceptance: failover.Unknown}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				actions []string
			)

			server := httptest.NewServer(
				http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
					var c balance.PrepaymentCommand
					if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
						t.Errorf("decode wallet command: %v", err)
					}

					mu.Lock()

					actions = append(actions, c.Action)
					mu.Unlock()
					rw.WriteHeader(http.StatusNotFound)
					_, _ = rw.Write([]byte(tc.body))
				}),
			)
			defer server.Close()

			balance.Default = balance.NewExternalHTTP(server.URL, "test")
			failedAt := time.Now().Add(-tc.failedAgo)
			task := &model.ImageTask{
				ID:                  "never-held-" + tc.name,
				GroupID:             "g",
				TokenID:             1,
				Status:              "failed",
				CompletedAt:         &failedAt,
				BillingOperationID:  "op",
				PrepaymentQuoteJSON: `{"version":1}`,
				Attempts:            tc.attempts,
				Error:               &model.ImageTaskError{Code: model.AsyncGenerationTimeoutCode},
			}
			require.NoError(t, db.Create(task).Error)
			_, err := imageprepayment.Sync(t.Context(), task)

			var saved model.ImageTask
			require.NoError(t, db.First(&saved, "id = ?", task.ID).Error)

			if tc.closed {
				require.NoError(t, err)
				require.Equal(t, []string{"settle"}, actions)
				require.True(t, saved.BillingSettled)
				require.Equal(t, "refunded", saved.Billing.Status)
				require.Zero(t, *saved.Billing.ActualMicros)
			} else {
				require.ErrorIs(t, err, balance.ErrPrepaymentNotFound)
				require.False(t, saved.BillingSettled)
			}
		})
	}
}
