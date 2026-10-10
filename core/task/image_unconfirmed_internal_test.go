package task

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// setupUnconfirmedImageTasks opens a fresh store and a fake application
// wallet that reports a hold refunded once it was settled.
func setupUnconfirmedImageTasks(t *testing.T) *deadlineWallet {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "unconfirmed.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}))
	oldLog, oldBalance := model.LogDB, balance.Default
	model.LogDB = db
	t.Cleanup(func() { model.LogDB, balance.Default = oldLog, oldBalance })
	wallet := &deadlineWallet{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c balance.PrepaymentCommand
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("decode wallet command: %v", err)
		}
		action := c.Action
		if c.Outcome != nil {
			action += ":" + c.Outcome.Kind + "/" + c.Outcome.Reason
		}
		wallet.mu.Lock()
		wallet.actions = append(wallet.actions, action)
		refunded := false
		for _, a := range wallet.actions {
			refunded = refunded || strings.HasPrefix(a, "settle:")
		}
		wallet.mu.Unlock()
		if refunded {
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":"` + c.BillingOperationID + `","status":"refunded","prepaidMicros":600000,"chargedMicros":0,"refundMicros":600000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":"` + c.BillingOperationID + `","status":"pending","prepaidMicros":600000}}`))
	}))
	t.Cleanup(server.Close)
	balance.Default = balance.NewExternalHTTP(server.URL, "test")
	return wallet
}

// storeImageTask reserves task id in status, reserved age ago. A prepaid task
// carries a quote and one attempt whose acceptance is unknown.
func storeImageTask(t *testing.T, id, status string, age time.Duration, prepaid bool) {
	t.Helper()
	task := &model.ImageTask{ID: id, GroupID: "g", TokenID: 1, Model: "m", UpstreamModel: "fal-ai/minimax/image-01", ChannelType: model.ChannelTypeFal}
	if prepaid {
		task.BillingOperationID = "op-" + id
		task.PrepaymentQuoteJSON = `{"version":1,"quoteVersion":"q","currency":"USD","prepaidMicros":600000,"routes":[{"routeId":"primary","channelId":1,"provider":"fal","endpoint":"fal-ai/minimax/image-01","credentialScope":"channel-1","estimatedMicros":400000,"prepaidMicros":600000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`
	}
	_, _, err := model.ReserveImageTask(task, &model.AsyncUsageInfo{RequestID: id, ChannelID: 1, GroupID: "g", TokenID: 1})
	require.NoError(t, err)
	changes := map[string]any{"created_at": time.Now().UTC().Add(-age)}
	if prepaid {
		changes["attempts"] = `[{"channel_id":1,"failure":{"acceptance":"unknown"}}]`
	}
	switch status {
	case "submission_unknown":
		require.NoError(t, model.SetImageTaskResult(id, "submission_unknown", nil, nil))
	case "queued":
		require.NoError(t, model.AcceptImageTask(id, "upstream-"+id))
	}
	require.NoError(t, model.LogDB.Model(&model.ImageTask{}).Where("id = ?", id).Updates(changes).Error)
}

func requireUnconfirmedTimeout(t *testing.T, id string) *model.ImageTask {
	t.Helper()
	saved, err := model.GetImageTask(id, "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Status)
	require.Equal(t, &model.ImageTaskError{Code: model.AsyncGenerationTimeoutCode, Message: model.AsyncGenerationTimeoutMessage}, saved.Error)
	require.Empty(t, saved.UpstreamID)
	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage, saved.UsageID).Error)
	require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
	var entry model.Log
	require.NoError(t, model.LogDB.First(&entry, usage.LogID).Error)
	require.Equal(t, model.AsyncGenerationTimeoutCode, entry.ErrorCode)
	require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
	return saved
}

// Owner rule 2026-10-08 on the image lane: a submission fal never confirmed
// fails 15 minutes after it started with generation_timeout and its hold is
// refunded in full in one wallet call. fal is never called: there is no ID to
// poll or cancel, and nothing is resubmitted.
func TestUnconfirmedImageSubmissionFailsAndRefunds(t *testing.T) {
	for _, status := range []string{"submission_unknown", "submitting"} {
		t.Run(status, func(t *testing.T) {
			wallet := setupUnconfirmedImageTasks(t)
			storeImageTask(t, "unconfirmed", status, model.AsyncGenerationDeadline+time.Second, true)
			expireUnconfirmedImageSubmissions(t.Context(), time.Now())
			saved := requireUnconfirmedTimeout(t, "unconfirmed")
			require.Equal(t, []string{"settle:failed/platform_failure", "get"}, wallet.list())
			require.NotNil(t, saved.Billing)
			require.Equal(t, "refunded", saved.Billing.Status)
			require.True(t, saved.BillingSettled)
			// A second pass finds nothing left to do.
			expireUnconfirmedImageSubmissions(t.Context(), time.Now().Add(time.Hour))
			require.Len(t, wallet.list(), 2)
		})
	}
}

// Legacy post-paid tasks hold nothing: they fail without any wallet call.
func TestUnconfirmedImageSubmissionWithoutPrepaymentFails(t *testing.T) {
	wallet := setupUnconfirmedImageTasks(t)
	storeImageTask(t, "legacy", "submission_unknown", time.Hour, false)
	expireUnconfirmedImageSubmissions(t.Context(), time.Now())
	saved := requireUnconfirmedTimeout(t, "legacy")
	require.Nil(t, saved.Billing)
	require.Empty(t, wallet.list())
}

// Young unconfirmed tasks and accepted tasks (which keep the provider
// deadline of processOneImageUsage) are never touched.
func TestUnconfirmedImageSubmissionScope(t *testing.T) {
	wallet := setupUnconfirmedImageTasks(t)
	storeImageTask(t, "young", "submission_unknown", model.AsyncGenerationDeadline-30*time.Second, true)
	storeImageTask(t, "accepted", "queued", time.Hour, true)
	expireUnconfirmedImageSubmissions(t.Context(), time.Now())
	young, err := model.GetImageTask("young", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "submission_unknown", young.Status)
	accepted, err := model.GetImageTask("accepted", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "queued", accepted.Status)
	require.Nil(t, accepted.Error)
	require.Empty(t, wallet.list())
	expired, err := model.ExpireUnconfirmedImageSubmission("accepted", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, expired)
}
