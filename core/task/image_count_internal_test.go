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

// Fewer valid outputs are billable; corrupt entries invalidate the whole result.
func TestImageWorkerSettlesActualValidImageCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected int
		urls     []string
		billed   int
	}{
		{"multiple", 3, []string{"https://cdn.example/1", "https://cdn.example/2", "https://cdn.example/3"}, 3},
		{"partial", 9, []string{"https://cdn.example/1", "https://cdn.example/2"}, 2},
		{"empty", 9, nil, 0},
		{"mixed invalid", 9, []string{"https://cdn.example/1", ""}, 0},
		{"excess", 1, []string{"https://cdn.example/1", "https://cdn.example/2"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "count.db"))
			require.NoError(t, err)
			require.NoError(
				t,
				db.AutoMigrate(
					&model.ImageTask{},
					&model.AsyncUsageInfo{},
					&model.Log{},
					&model.Channel{},
				),
			)

			oldDB, oldLog, oldBalance := model.DB, model.LogDB, balance.Default
			model.DB, model.LogDB = db, db
			t.Cleanup(func() { model.DB, model.LogDB, balance.Default = oldDB, oldLog, oldBalance })

			consumer := &replaySafeAmbiguousAsyncUsageConsumer{charges: map[string]int{}}
			balance.Default = replaySafeAmbiguousAsyncUsageBalance{consumer: consumer}

			require.NoError(
				t,
				model.CacheSetGroup(
					&model.GroupCache{ID: "image-count", Status: model.GroupStatusEnabled},
				),
			)
			t.Cleanup(func() { require.NoError(t, model.CacheDeleteGroup("image-count")) })

			outputs := make([]model.ImageOutput, 0, len(tc.urls))
			for _, u := range tc.urls {
				outputs = append(outputs, model.ImageOutput{URL: u})
			}

			polls := 0

			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					polls++

					var body any = map[string]any{"images": outputs}
					if strings.HasSuffix(r.URL.Path, "/status") {
						body = map[string]string{"status": "COMPLETED"}
					}

					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Errorf("write response: %v", err)
					}
				}),
			)
			defer server.Close()

			ch := &model.Channel{Type: model.ChannelTypeFal, Key: "key"}
			require.NoError(t, db.Create(ch).Error)
			info := &model.AsyncUsageInfo{
				RequestID:       "count",
				GroupID:         "image-count",
				TokenID:         1,
				ChannelID:       ch.ID,
				BaseURL:         server.URL,
				PricingCurrency: "USD",
				PricingVersion:  "release-1",
				Price:           model.Price{ImageOutputPrice: 0.25, ImageOutputPriceUnit: 1},
			}
			_, _, err = model.ReserveImageTask(
				&model.ImageTask{
					ID:             "count",
					GroupID:        "image-count",
					TokenID:        1,
					UpstreamModel:  "fal-ai/minimax/image-01/subject-reference",
					ExpectedImages: tc.expected,
				},
				info,
			)
			require.NoError(t, err)
			require.NoError(t, model.AcceptImageTask("count", "upstream"))

			runs := 1
			if tc.billed > 0 {
				runs = 2
			}

			for range runs {
				var fresh model.AsyncUsageInfo
				require.NoError(t, db.First(&fresh).Error)
				require.NoError(
					t,
					db.Model(&fresh).Update("next_poll_at", time.Now().Add(-time.Second)).Error,
				)
				fresh.NextPollAt = time.Now().Add(-time.Second)
				claimed, err := claimAsyncUsage(&fresh)
				require.NoError(t, err)
				require.True(t, claimed)
				processOneImageUsage(t.Context(), &fresh)
			}

			saved, err := model.GetImageTask("count", "image-count", 1)
			require.NoError(t, err)

			var final model.AsyncUsageInfo
			require.NoError(t, db.First(&final).Error)
			claimed, err := claimAsyncUsage(&final)
			require.NoError(t, err)
			require.False(t, claimed)
			require.Equal(t, 2, polls, "settlement replay uses stored result")

			if tc.billed == 0 {
				require.Equal(t, "failed", saved.Status)
				require.Empty(t, saved.Data)
				require.Zero(t, consumer.attempts)
				require.False(t, final.BalanceConsumed)
			} else {
				require.Equal(t, "completed", saved.Status)
				require.Len(t, saved.Data, tc.billed)
				require.Equal(t, 1, consumer.charges["count"])
				require.Equal(
					t,
					[]float64{float64(tc.billed) * 0.25, float64(tc.billed) * 0.25},
					consumer.amounts,
				)

				var entry model.Log
				require.NoError(t, db.Where("request_id = ?", "count").First(&entry).Error)
				require.EqualValues(t, tc.billed, entry.Usage.ImageOutputTokens)
				require.InDelta(t, float64(tc.billed)*0.25, entry.Amount.UsedAmount, 0.000001)
			}
		})
	}
}
