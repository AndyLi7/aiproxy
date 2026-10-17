package controller

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestImageSubmissionReceiptActualHTTP(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, fault := range []string{"none", "terminal", "price", "operation", "save", "accept"} {
			name := fault
			if active {
				name = "active/" + name
			}
			t.Run(name, func(t *testing.T) {
				h := newImageCallLogHarness(t, http.StatusOK)
				if active {
					activateImageAdmission(t, h)
				}
				var sent, failed atomic.Bool
				h.providerHook = func() {
					sent.Store(true)
					switch fault {
					case "terminal":
						require.NoError(t, model.SetImageTaskResult(h.requestID, "failed", nil, &model.ImageTaskError{Code: "submission_rejected"}))
					case "price":
						require.NoError(t, h.db.Model(&model.AsyncUsageInfo{}).Where("image_task_id=?", h.requestID).Update("per_request_price", 9).Error)
					case "operation":
						require.NoError(t, h.db.Model(&model.ImageTask{}).Where("id=?", h.requestID).Update("billing_operation_id", "changed-operation").Error)
					}
				}
				require.NoError(t, h.db.Callback().Update().Before("gorm:update").Register("receipt:fail-write", func(tx *gorm.DB) {
					if !sent.Load() || tx.Statement.Table != "image_tasks" {
						return
					}
					m, ok := tx.Statement.Dest.(map[string]interface{})
					if !ok {
						return
					}
					_, attempt := m["attempts"]
					if ((fault == "save" && attempt) || (fault == "accept" && m["status"] == "queued")) && failed.CompareAndSwap(false, true) {
						tx.AddError(errors.New("synthetic receipt failure"))
					}
				}))
				response := h.submit()
				require.Equal(t, 1, h.providerCalls)
				var task model.ImageTask
				require.NoError(t, h.db.First(&task, "id=?", h.requestID).Error)
				var usage model.AsyncUsageInfo
				require.NoError(t, h.db.First(&usage, task.UsageID).Error)
				if fault == "none" {
					require.Equal(t, 202, response.Code, response.Body.String())
					require.Equal(t, "queued", task.Status)
					require.Equal(t, model.AsyncUsageStatusPending, usage.Status)
					require.EqualValues(t, 1, usage.Price.PerRequestPrice)
				} else {
					require.Equal(t, 503, response.Code, response.Body.String())
					require.Empty(t, task.UpstreamID)
					if fault == "terminal" {
						require.Equal(t, "failed", task.Status)
						require.Equal(t, model.AsyncUsageStatusFailed, usage.Status)
					} else {
						require.Equal(t, "submitting", task.Status)
						require.Equal(t, model.AsyncUsageStatusNone, usage.Status)
					}
				}
				require.Len(t, h.commands, 2, "only original admit+begin, no new settlement/refund action")
				require.Equal(t, 202, h.submit().Code)
				require.Equal(t, 1, h.providerCalls)
				require.Len(t, h.commands, 2)
				if active {
					for _, use := range imageAdmissionUses(t, h) {
						require.Equal(t, "open", use.State)
					}
				}
			})
		}
	}
}
