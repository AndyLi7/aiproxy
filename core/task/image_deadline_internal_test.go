package task

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// deadlineWallet is a fake application wallet that records prepayment actions.
type deadlineWallet struct {
	mu      sync.Mutex
	actions []string
}

func (w *deadlineWallet) list() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.actions...)
}

func (w *deadlineWallet) refunded() bool {
	for _, a := range w.list() {
		if a == "settle:failed/platform_failure" {
			return true
		}
	}
	return false
}

// deadlineFal is a fake fal queue for the task's legacy app-root paths.
type deadlineFal struct {
	mu           sync.Mutex
	calls        []string
	status       string
	statusCode   int
	cancelStatus int
	cancels      int
	// refundedAtCancel records whether the refund preceded each cancel.
	wallet           *deadlineWallet
	refundedAtCancel []bool
}

func (f *deadlineFal) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Key secret" {
			t.Errorf("fal called with another credential: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/fal-ai/minimax/requests/upstream/status":
			if f.statusCode != 0 {
				w.WriteHeader(f.statusCode)
				return
			}
			_, _ = w.Write([]byte(`{"status":"` + f.status + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/fal-ai/minimax/requests/upstream":
			_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn.example/a.png"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/fal-ai/minimax/requests/upstream/cancel":
			f.cancels++
			if f.wallet != nil {
				f.refundedAtCancel = append(f.refundedAtCancel, f.wallet.refunded())
			}
			if f.cancelStatus != 0 {
				w.WriteHeader(f.cancelStatus)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"CANCELLATION_REQUESTED"}`))
		default:
			t.Errorf("unexpected fal call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *deadlineFal) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

// setupDeadlineTask stores an accepted fal image task whose acceptance was age
// ago. A prepaid task gets a D34 quote and the fake wallet as balance.Default.
func setupDeadlineTask(t *testing.T, age time.Duration, status string, fal *deadlineFal, wallet *deadlineWallet) {
	t.Helper()
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "deadline.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}, &model.AsyncUsageInfo{}, &model.Log{}, &model.Channel{}))
	oldDB, oldLog, oldBalance := model.DB, model.LogDB, balance.Default
	model.DB, model.LogDB = db, db
	t.Cleanup(func() { model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldBalance })

	falServer := httptest.NewServer(fal.handler(t))
	t.Cleanup(falServer.Close)
	ch := &model.Channel{Type: model.ChannelTypeFal, Key: "secret"}
	require.NoError(t, db.Create(ch).Error)
	info := &model.AsyncUsageInfo{RequestID: "deadline", ChannelID: ch.ID, BaseURL: falServer.URL, GroupID: "g", TokenID: 1}
	task := &model.ImageTask{
		ID: "deadline", GroupID: "g", TokenID: 1, UpstreamModel: "fal-ai/minimax/image-01",
		KeyFingerprint: model.ImageChannelKeyFingerprint("secret"), ChannelType: model.ChannelTypeFal,
	}
	if wallet != nil {
		walletServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			refunded := strings.HasPrefix(action, "settle:")
			for _, a := range wallet.actions {
				refunded = refunded || strings.HasPrefix(a, "settle:")
			}
			wallet.mu.Unlock()
			if refunded {
				_, _ = w.Write([]byte(`{"code":0,"data":{"id":"op-deadline","status":"refunded","prepaidMicros":600000,"chargedMicros":0,"refundMicros":600000}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":"op-deadline","status":"pending","prepaidMicros":600000}}`))
		}))
		t.Cleanup(walletServer.Close)
		balance.Default = balance.NewExternalHTTP(walletServer.URL, "test")
		task.BillingOperationID = "op-deadline"
		task.PrepaymentQuoteJSON = `{"version":1,"quoteVersion":"q","currency":"USD","prepaidMicros":600000,"routes":[{"routeId":"primary","channelId":1,"provider":"fal","endpoint":"fal-ai/minimax/image-01","credentialScope":"channel-1","estimatedMicros":400000,"prepaidMicros":600000,"rule":{"mode":"list_ratio","ratio":"1"}}]}`
	}
	_, _, err = model.ReserveImageTask(task, info)
	require.NoError(t, err)
	if wallet != nil {
		require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", "deadline").Update("attempts", `[{"channel_id":1,"failure":{"acceptance":"accepted"}}]`).Error)
	}
	require.NoError(t, model.AcceptImageTask("deadline", "upstream"))
	require.NoError(t, db.Model(&model.ImageTask{}).Where("id = ?", "deadline").
		Updates(map[string]any{"status": status, "queued_at": time.Now().UTC().Add(-age)}).Error)
}

func claimDeadlineUsage(t *testing.T) *model.AsyncUsageInfo {
	t.Helper()
	info := &model.AsyncUsageInfo{}
	require.NoError(t, model.LogDB.First(info).Error)
	claimed, err := claimAsyncUsage(info)
	require.NoError(t, err)
	require.True(t, claimed)
	return info
}

// requireGenerationTimeout checks the durable outcome customers and the
// application see: the task, its accounting outbox and its request log.
func requireGenerationTimeout(t *testing.T, info *model.AsyncUsageInfo) *model.ImageTask {
	t.Helper()
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Status)
	require.Equal(t, &model.ImageTaskError{Code: model.AsyncGenerationTimeoutCode, Message: model.AsyncGenerationTimeoutMessage}, saved.Error)
	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
	require.Empty(t, usage.ProcessingToken)
	var entry model.Log
	require.NoError(t, model.LogDB.First(&entry, usage.LogID).Error)
	require.Equal(t, model.AsyncGenerationTimeoutCode, entry.ErrorCode)
	require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
	require.Equal(t, 502, entry.Code)
	claimed, err := claimAsyncUsage(&usage)
	require.NoError(t, err)
	require.False(t, claimed, "a timed-out task is never polled again")
	return saved
}

// requireTimeoutRefund checks the wallet contract: one full platform_failure
// refund in a single call (no execution_finished, which would open a window for
// an actual-cost charge), and nothing is ever delivered.
func requireTimeoutRefund(t *testing.T, wallet *deadlineWallet, saved *model.ImageTask) {
	t.Helper()
	require.Equal(t, []string{"accept_attempt", "settle:failed/platform_failure", "get"}, wallet.list())
	require.NotNil(t, saved.Billing)
	require.Equal(t, "refunded", saved.Billing.Status)
	require.True(t, saved.BillingSettled)
}

// Owner rule 2026-10-08: 15 minutes after acceptance with no result, the task
// fails with generation_timeout whatever the last poll said (still queued,
// still running, or an error), the hold is refunded, and only then is fal
// asked once to cancel.
func TestImageGenerationDeadlineFailsRefundsAndCancels(t *testing.T) {
	for name, fal := range map[string]*deadlineFal{
		"queued":      {status: "IN_QUEUE"},
		"in_progress": {status: "IN_PROGRESS"},
		"poll error":  {statusCode: http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			wallet := &deadlineWallet{}
			fal.wallet = wallet
			setupDeadlineTask(t, model.AsyncGenerationDeadline+time.Second, "in_progress", fal, wallet)
			info := claimDeadlineUsage(t)
			processOneImageUsage(t.Context(), info)
			saved := requireGenerationTimeout(t, info)
			requireTimeoutRefund(t, wallet, saved)
			require.Equal(t, 1, fal.cancelCount())
			require.Equal(t, []bool{true}, fal.refundedAtCancel, "the cancel never delays the refund")
			require.Equal(t, "GET /fal-ai/minimax/requests/upstream/status", fal.calls[0], "the provider is polled once more first")
		})
	}
}

// Legacy post-paid and internal tasks have no hold: they fail and cancel, and
// no wallet is called (nothing is ever charged for them).
func TestImageGenerationDeadlineWithoutPrepaymentFailsAndCancels(t *testing.T) {
	fal := &deadlineFal{status: "IN_QUEUE"}
	setupDeadlineTask(t, time.Hour, "queued", fal, nil)
	info := claimDeadlineUsage(t)
	processOneImageUsage(t.Context(), info)
	saved := requireGenerationTimeout(t, info)
	require.Nil(t, saved.Billing)
	require.Equal(t, 1, fal.cancelCount())
}

// Cancellation is best effort: a failing cancel is logged, never retried, and
// changes neither the failure nor the refund.
func TestImageGenerationDeadlineCancelFailureNeverBlocksRefund(t *testing.T) {
	wallet := &deadlineWallet{}
	fal := &deadlineFal{status: "IN_PROGRESS", cancelStatus: http.StatusInternalServerError, wallet: wallet}
	setupDeadlineTask(t, 20*time.Minute, "in_progress", fal, wallet)
	info := claimDeadlineUsage(t)
	processOneImageUsage(t.Context(), info)
	saved := requireGenerationTimeout(t, info)
	requireTimeoutRefund(t, wallet, saved)
	require.Equal(t, 1, fal.cancelCount())
}

// A rotated credential is never replaced by another key: the task still fails
// and refunds, and fal receives nothing at all.
func TestImageGenerationDeadlineRotatedCredentialNeverCancels(t *testing.T) {
	wallet := &deadlineWallet{}
	fal := &deadlineFal{status: "IN_PROGRESS"}
	setupDeadlineTask(t, 20*time.Minute, "in_progress", fal, wallet)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("key", "rotated").Error)
	info := claimDeadlineUsage(t)
	processOneImageUsage(t.Context(), info)
	saved := requireGenerationTimeout(t, info)
	requireTimeoutRefund(t, wallet, saved)
	require.Empty(t, fal.calls)
}

// The final poll still happens at the deadline; a result fal finished in time
// is delivered, never expired, refunded or cancelled.
func TestImageGenerationDeadlineDeliversResultFoundAtTheDeadline(t *testing.T) {
	fal := &deadlineFal{status: "COMPLETED"}
	setupDeadlineTask(t, time.Hour, "in_progress", fal, nil)
	processOneImageUsage(t.Context(), claimDeadlineUsage(t))
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "completed", saved.Status)
	require.Nil(t, saved.Error)
	require.Zero(t, fal.cancelCount())
}

// A younger task keeps polling and is never cancelled, and while fal keeps
// erroring its next poll is pulled in to just past the deadline instead of a
// full backoff later.
func TestImageGenerationDeadlineSchedulesAPollAtTheDeadline(t *testing.T) {
	fal := &deadlineFal{statusCode: http.StatusBadGateway}
	setupDeadlineTask(t, model.AsyncGenerationDeadline-30*time.Second, "in_progress", fal, nil)
	info := claimDeadlineUsage(t)
	info.RetryCount = 9 // the next backoff is the 3-minute maximum
	started := time.Now()
	processOneImageUsage(t.Context(), info)
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "in_progress", saved.Status)
	require.Zero(t, fal.cancelCount())
	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
	deadline := model.ImageGenerationDeadline(saved)
	require.True(t, usage.NextPollAt.After(deadline), "never polled before the deadline, which would only poll again")
	require.False(t, usage.NextPollAt.After(deadline.Add(time.Second)), "next poll %s, deadline %s", usage.NextPollAt, deadline)
	require.True(t, usage.NextPollAt.After(started.Add(20*time.Second)))
}

// A healthy young task is untouched by the rule.
func TestImageGenerationDeadlineLeavesYoungTasksPolling(t *testing.T) {
	fal := &deadlineFal{status: "IN_PROGRESS"}
	setupDeadlineTask(t, 5*time.Minute, "in_progress", fal, nil)
	info := claimDeadlineUsage(t)
	processOneImageUsage(t.Context(), info)
	saved, err := model.GetImageTask("deadline", "g", 1)
	require.NoError(t, err)
	require.Equal(t, "in_progress", saved.Status)
	require.Nil(t, saved.Error)
	require.Zero(t, fal.cancelCount())
	var usage model.AsyncUsageInfo
	require.NoError(t, model.LogDB.First(&usage, info.ID).Error)
	require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
}
