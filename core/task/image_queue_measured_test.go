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
func TestImageQueueMeasuredSettlementPersistsAndReplays(t *testing.T) {
	for _, tc := range []struct {
		name        string
		missing     bool
		legacy      bool
		staleFailed bool
		expected    int
		urls        []string
		billed      int
	}{
		{"measured", false, false, false, 3, []string{"https://cdn.example/1", "https://cdn.example/2", "https://cdn.example/3"}, 3},
		{"partial", false, false, false, 9, []string{"https://cdn.example/1", "https://cdn.example/2"}, 2},
		{"missing", true, false, false, 9, []string{"https://cdn.example/1"}, 0},
		{"legacy with metering", false, true, false, 3, []string{"https://cdn.example/1"}, 1},
		{"mixed invalid", false, false, false, 9, []string{"https://cdn.example/1", ""}, 0},
		{"excess", false, false, false, 1, []string{"https://cdn.example/1", "https://cdn.example/2"}, 0},
		{"stale failed poll after durable completion", false, false, true, 1, []string{"https://cdn.example/1"}, 1},
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
				w, h := int64(10), int64(20)
				img := model.ImageOutput{URL: u, Width: &w, Height: &h}
				if tc.missing {
					img.Width = nil
				}
				outputs = append(outputs, img)
			}

			polls := 0

			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					polls++

					var body any = map[string]any{"images": outputs}
					if strings.HasSuffix(r.URL.Path, "/status") {
						body = map[string]string{"status": "COMPLETED"}
						if tc.staleFailed {
							if err := model.SetImageTaskResult("count", "completed", outputs, nil); err != nil {
								t.Errorf("save competing result: %v", err)
							}
							body = map[string]string{"status": "FAILED"}
						}
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
			inputs := int64(2)
			limit := int64(100)
			info.UsageContext.ImageUsage = &model.ImageUsage{Version: 1, State: "incomplete", Scenario: "generation", InputCount: &inputs, Outputs: []model.ImageUsageOutput{}}
			if !tc.legacy {
				info.Price.ImageBilling = &model.ImageBillingPolicy{Version: 1, Scenario: "generation", Input: &model.ImageBillingInput{ChargeBasis: "per_output", FirstNFree: 1, AmountMicros: 50000, UnitQuantity: 1}, OutputPixelTiers: []model.ImageBillingTier{{MaxPixels: &limit, AmountMicros: 100000, UnitQuantity: 1}, {AmountMicros: 200000, UnitQuantity: 1}}}
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
			expectedPolls := 2
			if tc.staleFailed {
				expectedPolls = 1
			}
			require.Equal(t, expectedPolls, polls, "settlement replay uses stored result")

			if tc.billed == 0 {
				if tc.missing {
					require.Equal(t, "completed", saved.Status)
					require.Equal(t, model.AsyncUsageStatusMeasurementPending, final.Status)
					require.NotNil(t, final.Amount.ImageBillingResult)
					var entry model.Log
					require.NoError(t, db.First(&entry, final.LogID).Error)
					require.Equal(t, "pending", entry.Amount.ImageBillingResult.State)
					require.Len(t, entry.UsageContext.ImageUsage.Outputs, 1)
					return
				}
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
				require.Len(t, entry.UsageContext.ImageUsage.Outputs, tc.billed)
				require.Equal(t, int64(2), *entry.UsageContext.ImageUsage.InputCount)
				require.Equal(t, int64(10), *entry.UsageContext.ImageUsage.Outputs[0].Width)
			}
		})
	}
}
