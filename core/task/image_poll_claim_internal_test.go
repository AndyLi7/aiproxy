package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestImagePollLostClaimEntryNeverCallsProviderOrWallet(t *testing.T) {
	for _, state := range []string{"in_progress", "failed", "completed", "result_processing"} {
		t.Run(state, func(t *testing.T) {
			for _, mode := range []string{"expired", "owner"} {
				t.Run(mode, func(t *testing.T) {
					wallet := &deadlineWallet{}
					fal := &deadlineFal{status: "COMPLETED"}
					setupDeadlineTask(t, time.Minute, state, fal, wallet)

					info := claimDeadlineUsage(t)
					if mode == "expired" {
						require.NoError(
							t,
							model.LogDB.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("next_poll_at", time.Now().Add(-time.Second)).
								Error,
						)
					} else {
						require.NoError(
							t,
							model.LogDB.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("processing_token", "replacement").
								Error,
						)
					}

					processOneImageUsage(t.Context(), info)
					require.Empty(t, fal.calls)
					require.Empty(t, wallet.list())

					saved, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
					require.NoError(t, err)
					require.Equal(t, state, saved.Status)

					var entry model.Log
					require.NoError(t, model.LogDB.First(&entry, info.LogID).Error)
					require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
				})
			}
		})
	}
}

func TestImagePollClaimChangeBetweenRequestsAndReceipt(t *testing.T) {
	for _, point := range []string{"status", "result", "failure"} {
		t.Run(point, func(t *testing.T) {
			for _, mode := range []string{"expired", "owner", "renewed"} {
				t.Run(mode, func(t *testing.T) {
					wallet := &deadlineWallet{}
					fal := &deadlineFal{}
					setupDeadlineTask(t, time.Minute, "in_progress", fal, wallet)
					info := claimDeadlineUsage(t)
					calls := 0

					server := httptest.NewServer(
						http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls++

							isStatus := strings.HasSuffix(r.URL.Path, "/status")
							if (point == "result" && !isStatus) || (point != "result" && isStatus) {
								query := model.LogDB.Model(&model.AsyncUsageInfo{}).
									Where("id=?", info.ID)

								var err error
								switch mode {
								case "expired":
									err = query.Update(
										"next_poll_at",
										time.Now().Add(-time.Second),
									).Error
								case "owner":
									err = query.Update("processing_token", "replacement").Error
								case "renewed":
									_, err = model.RenewAsyncUsageClaim(
										info.ID,
										info.ProcessingToken,
										time.Now().Add(5*time.Minute),
									)
								}

								if err != nil {
									t.Error(err)
								}
							}

							if isStatus {
								if point == "failure" {
									_, _ = w.Write([]byte(`{"status":"FAILED"}`))
								} else {
									_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
								}
							} else {
								_, _ = w.Write(
									[]byte(`{"images":[{"url":"https://cdn.example/result.png"}]}`),
								)
							}
						}),
					)
					defer server.Close()

					info.BaseURL = server.URL
					processOneImageUsage(t.Context(), info)
					saved, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
					require.NoError(t, err)

					if mode == "renewed" {
						if point == "failure" {
							require.Equal(t, "failed", saved.Status)
							requireFailedImageRefund(t, wallet)
						} else {
							require.Equal(t, "completed", saved.Status)
						}
					} else {
						require.Equal(t, "in_progress", saved.Status)
						require.Empty(t, wallet.list())

						if point == "status" {
							require.Equal(t, 1, calls, "no result request after losing claim")
						}
					}
				})
			}
		})
	}
}

func TestImagePollArchiveLostClaimRejectsReceiptAndNextAction(t *testing.T) {
	for _, mode := range []string{"expired", "owner", "live"} {
		t.Run(mode, func(t *testing.T) {
			setupDeadlineTask(t, time.Minute, "in_progress", &deadlineFal{}, nil)
			info := claimDeadlineUsage(t)
			require.NoError(
				t,
				model.SetImageTaskResult(
					info.ImageTaskID,
					"result_processing",
					[]model.ImageOutput{
						{URL: "https://cdn.example/1.png"},
						{URL: "https://cdn.example/2.png"},
					},
					nil,
				),
			)
			task, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
			require.NoError(t, err)
			ctx := actionguard.With(
				t.Context(),
				func(ctx context.Context) error { return model.ValidateClaimedImageTask(ctx, info, task) },
			)
			calls := 0
			err = archiveClaimedImageTask(
				ctx,
				info,
				task,
				func(context.Context, string) (string, ownedimage.Metadata, error) {
					calls++

					switch mode {
					case "expired":
						require.NoError(
							t,
							model.LogDB.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("next_poll_at", time.Now().Add(-time.Second)).
								Error,
						)
					case "owner":
						require.NoError(
							t,
							model.LogDB.Model(&model.AsyncUsageInfo{}).
								Where("id=?", info.ID).
								Update("processing_token", "replacement").
								Error,
						)
					}

					return "https://owned.example/image.png", ownedimage.Metadata{
						ContentType: "image/png",
					}, nil
				},
			)
			saved, e := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
			require.NoError(t, e)

			if mode == "live" {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, "completed", saved.Status)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, calls)
				require.False(t, saved.Data[0].Stored)
				require.False(t, task.Data[0].Stored)
			}
		})
	}
}

func TestImagePollInvalidStoredCountFencesFailureReceipt(t *testing.T) {
	for _, mode := range []string{"live", "expired", "owner", "expired-write"} {
		t.Run(mode, func(t *testing.T) {
			wallet := &deadlineWallet{}
			fal := &deadlineFal{}
			setupDeadlineTask(t, time.Minute, "completed", fal, wallet)
			info := claimDeadlineUsage(t)
			deadline := time.Now().Add(350 * time.Millisecond)

			query := model.LogDB.Model(&model.AsyncUsageInfo{}).Where("id=?", info.ID)
			switch mode {
			case "expired":
				require.NoError(t, query.Update("next_poll_at", time.Now().Add(-time.Second)).Error)
			case "owner":
				require.NoError(t, query.Update("processing_token", "replacement").Error)
			case "expired-write":
				require.NoError(t, query.Update("next_poll_at", deadline).Error)

				require.NoError(
					t,
					model.LogDB.Callback().
						Update().
						After("gorm:update").
						Register("test:invalid-expiry", func(tx *gorm.DB) { time.Sleep(time.Until(deadline.Add(20 * time.Millisecond))) }),
				)
				defer func() {
					if err := model.LogDB.Callback().
						Update().
						Remove("test:invalid-expiry"); err != nil {
						t.Error(err)
					}
				}()
			}

			processOneImageUsage(t.Context(), info)

			var usage model.AsyncUsageInfo
			require.NoError(t, model.LogDB.First(&usage, info.ID).Error)

			var entry model.Log
			require.NoError(t, model.LogDB.First(&entry, info.LogID).Error)

			if mode == "live" {
				require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
				require.Equal(t, model.AsyncUsageStatusFailed, entry.AsyncUsageStatus)
				require.Empty(t, usage.ProcessingToken)
			} else {
				require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
				require.Equal(t, model.AsyncUsageStatusPending, entry.AsyncUsageStatus)
				require.NotEmpty(t, usage.ProcessingToken)
			}

			saved, err := model.GetImageTask(info.ImageTaskID, info.GroupID, info.TokenID)
			require.NoError(t, err)
			require.Equal(t, "completed", saved.Status)
			require.Empty(t, fal.calls)
			require.Empty(t, wallet.list())
		})
	}
}
