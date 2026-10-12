//nolint:testpackage // recordResult is the relay's internal result recorder.
package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/notify"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

type capturedNotification struct {
	key   string
	title string
}

type captureNotifier struct {
	mu   sync.Mutex
	sent []capturedNotification
}

func (n *captureNotifier) Notify(level notify.Level, title, message string) {
	n.NotifyThrottle(level, "", 0, title, message)
}

func (n *captureNotifier) NotifyThrottle(_ notify.Level, key string, _ time.Duration, title, _ string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.sent = append(n.sent, capturedNotification{key: key, title: title})
}

func (n *captureNotifier) keys() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	keys := make([]string, 0, len(n.sent))
	for _, sent := range n.sent {
		keys = append(keys, sent.key)
	}

	return keys
}

// A video-like producer on a mode classified as sync-call would show unfinished
// jobs as succeeded and never as unknown. recordResult reports any such
// producer at run time; the row is still written as pending.
func TestAsyncUsageOnSyncModeAlerts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "async-usage-mode.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldLogDB })

	notifier := &captureNotifier{}
	notify.SetDefaultNotifier(notifier)
	t.Cleanup(func() { notify.SetDefaultNotifier(&notify.StdNotifier{}) })

	record := func(relayMode mode.Mode) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", nil)
		c.Set(middleware.GroupBalance, &middleware.GroupBalanceConsumer{})

		m := &meta.Meta{
			RequestID:   "async-" + relayMode.String(),
			Mode:        relayMode,
			OriginModel: "model",
			Group:       model.GroupCache{ID: "g"},
			Token:       model.TokenCache{ID: 1},
		}
		recordResult(c, m, model.Price{}, &relaycontroller.HandleResult{AsyncUsage: true}, 0, true, nil)

		// Consume writes the row in the background; it stays pending.
		require.Eventually(t, func() bool {
			var entry model.Log
			return db.Where("request_id = ?", m.RequestID).First(&entry).Error == nil &&
				entry.AsyncUsageStatus == model.AsyncUsageStatusPending
		}, 5*time.Second, 10*time.Millisecond)
	}

	record(mode.Embeddings)
	require.Equal(t, []string{"notifylimit:error:async-usage-mode-Embeddings"}, notifier.keys())

	// Async-submit modes and the reviewed billing-only modes are expected.
	record(mode.Videos)
	record(mode.NativeTasks)
	record(mode.ChatCompletions)
	require.Equal(t, []string{"notifylimit:error:async-usage-mode-Embeddings"}, notifier.keys())
}
