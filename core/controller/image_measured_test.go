//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor/doubao"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecordMeasuredResultIsDurableBeforeReturning(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		nil,
	)
	m := &meta.Meta{
		RequestID:   "measured-final",
		OriginModel: "image",
		Group:       model.GroupCache{ID: "g"},
		Token:       model.TokenCache{ID: 1},
	}
	price := model.Price{
		OutputPrice:     1,
		OutputPriceUnit: 1,
		ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
	}
	recordResult(c, m, price, &relaycontroller.HandleResult{}, 0, true, nil)

	var entry model.Log
	require.NoError(t, db.Where("request_id = ?", m.RequestID).First(&entry).Error)
	require.Equal(t, model.AsyncUsageStatusMeasurementPending, entry.AsyncUsageStatus)
	require.Equal(t, "pending", entry.Amount.ImageBillingResult.State)

	var info model.AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.True(t, info.MeasuredImage)
	require.Equal(t, entry.ID, info.LogID)
}

func TestMeasuredFailedAttemptCannotChargeReturnedImages(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", nil)

	price := model.Price{
		OutputPrice:     1,
		OutputPriceUnit: 1,
		ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
	}
	for _, downstream := range []bool{false, true} {
		m := &meta.Meta{RequestID: strconv.FormatBool(downstream), OriginModel: "image"}
		w := int64(10)

		result := &relaycontroller.HandleResult{
			UsageContext: model.UsageContext{
				ImageUsage: &model.ImageUsage{
					Version:  1,
					State:    "complete",
					Scenario: "generation",
					Outputs:  []model.ImageUsageOutput{{Index: 0, Width: &w, Height: &w}},
				},
			},
		}
		if downstream {
			result.Error = relaymodel.WrapperOpenAIError(
				errors.New("failed"),
				"upstream_failed",
				502,
			)
		}

		recordResult(c, m, price, result, 0, downstream, nil)

		var info model.AsyncUsageInfo
		require.NoError(t, db.Where("request_id = ?", m.RequestID).First(&info).Error)
		require.Equal(t, model.AsyncUsageStatusFailed, info.Status)
		require.Zero(t, info.Amount.UsedAmount)
		require.Equal(t, "failed", info.UsageContext.ImageUsage.State)

		var entries []model.Log
		require.NoError(t, db.Where("request_id = ?", m.RequestID).Find(&entries).Error)

		if !downstream {
			// A non-final attempt is evidence only: no request-log row, no charge.
			require.Empty(t, entries)
			require.Zero(t, info.LogID)

			continue
		}

		require.Len(t, entries, 1)
		require.Equal(t, entries[0].ID, info.LogID)
		require.Equal(t, model.AsyncUsageStatusFailed, entries[0].AsyncUsageStatus)
		require.Zero(t, entries[0].Amount.UsedAmount)
		require.Equal(t, "failed", entries[0].UsageContext.ImageUsage.State)
	}
}

// One HTTP request is one call row. A failed attempt that is retried on another
// channel used to write its own logs row, so the request counted twice.
func TestMeasuredRetryWritesOneCallRow(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.RetryLog{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	oldRetryHours := config.GetRetryLogStorageHours()

	config.SetRetryLogStorageHours(24)
	t.Cleanup(func() { config.SetRetryLogStorageHours(oldRetryHours) })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		nil,
	)

	price := model.Price{
		OutputPrice:     1,
		OutputPriceUnit: 1,
		ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
	}
	side := int64(10)
	attempt := func(channelID int, downstream bool, failure error) {
		m := &meta.Meta{
			RequestID:   "measured-retry",
			OriginModel: "image",
			Group:       model.GroupCache{ID: "g"},
			Token:       model.TokenCache{ID: 3, Name: "prod"},
			Channel:     meta.ChannelMeta{ID: channelID},
		}

		result := &relaycontroller.HandleResult{
			UsageContext: model.UsageContext{ImageUsage: &model.ImageUsage{
				Version:  1,
				State:    "complete",
				Scenario: "generation",
				Outputs:  []model.ImageUsageOutput{{Index: 0, Width: &side, Height: &side}},
			}},
		}
		if failure != nil {
			result.Error = relaymodel.WrapperOpenAIError(
				failure,
				"upstream_failed",
				http.StatusBadGateway,
			)
		}

		recordResult(c, m, price, result, channelID-1, downstream, nil)
	}

	attempt(1, false, errors.New("first channel failed"))
	attempt(2, true, nil)

	var entries []model.Log
	require.NoError(t, db.Where("request_id = ?", "measured-retry").Find(&entries).Error)
	require.Len(t, entries, 1, "one HTTP request must write one call row")
	require.Equal(t, http.StatusOK, entries[0].Code)
	require.Equal(t, 2, entries[0].ChannelID)
	require.True(t, entries[0].IsCall())

	var infos []model.AsyncUsageInfo
	require.NoError(t, db.Where("request_id = ?", "measured-retry").Order("id").Find(&infos).Error)
	require.Len(t, infos, 2)
	require.Zero(t, infos[0].LogID)
	require.Equal(t, model.AsyncUsageStatusFailed, infos[0].Status)
	require.Zero(t, infos[0].Amount.UsedAmount)
	require.Equal(t, entries[0].ID, infos[1].LogID)

	var retries []model.RetryLog
	require.NoError(t, db.Where("request_id = ?", "measured-retry").Find(&retries).Error)
	require.Len(t, retries, 1)
	require.Equal(t, 1, retries[0].ChannelID)
}

func TestMeasuredLogsPreserveConfiguredDetailsAndRetention(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(
		t,
		db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}, &model.RequestDetail{}),
	)

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	oldHours, oldLogHours := config.GetLogDetailStorageHours(), config.GetLogStorageHours()
	t.Cleanup(
		func() { config.SetLogDetailStorageHours(oldHours); config.SetLogStorageHours(oldLogHours) },
	)

	for _, tc := range []struct {
		name                  string
		code                  int
		detailHours, logHours int64
		retain                bool
	}{{"configured", 200, 24, 24, true}, {"error", 502, 24, 24, true}, {"429", 429, 24, 24, false}, {"detail disabled", 200, -1, 24, false}, {"log disabled", 200, 24, -1, false}} {
		config.SetLogDetailStorageHours(tc.detailHours)
		config.SetLogStorageHours(tc.logHours)

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", nil)
		m := &meta.Meta{
			RequestID: tc.name,
			ModelConfig: model.ModelConfig{
				ForceSaveDetail:            tc.code == 200,
				RequestBodyStorageMaxSize:  16,
				ResponseBodyStorageMaxSize: 16,
			},
		}

		result := &relaycontroller.HandleResult{
			BodyDetail: &relaycontroller.BodyDetail{
				RequestBody:  "private-request-content",
				ResponseBody: "private-response-content",
			},
		}
		if tc.code != 200 {
			result.Error = relaymodel.WrapperOpenAIError(
				errors.New("failed"),
				"upstream_failed",
				tc.code,
			)
		}

		price := model.Price{
			ImageBilling: &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
		}
		recordResult(c, m, price, result, 0, true, nil)

		var entry model.Log
		require.NoError(
			t,
			db.Preload("RequestDetail").Where("request_id = ?", tc.name).First(&entry).Error,
		)

		if tc.retain {
			require.NotNil(t, entry.RequestDetail)
			require.Equal(t, entry.ID, entry.RequestDetail.LogID)
			require.True(t, entry.RequestDetail.ResponseBodyTruncated)
			require.LessOrEqual(t, len(entry.RequestDetail.ResponseBody), 16)
		} else {
			require.Nil(t, entry.RequestDetail)
		}

		b, err := json.Marshal(entry.UsageContext.ImageUsage)
		require.NoError(t, err)
		require.NotContains(t, string(b), "private-")
	}
}

func TestMeasuredDoubaoRequiresInputCountWithFreeRetailInputs(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	price := model.Price{
		OutputPrice:     .3,
		OutputPriceUnit: 1,
		ImageBilling: &model.ImageBillingPolicy{
			Version:  1,
			Scenario: "generation",
			Input:    &model.ImageBillingInput{FirstNFree: 1, AmountMicros: 0, UnitQuantity: 1},
		},
	}
	for _, tc := range []struct {
		name, usage string
		missing     bool
	}{{"missing", `{"generated_images":1}`, true}, {"null", `{"input_images":null,"generated_images":1}`, true}, {"zero", `{"input_images":0,"generated_images":1}`, false}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", nil)
		m := meta.NewMeta(
			&model.Channel{},
			5,
			"measured",
			model.ModelConfig{Price: price},
			meta.WithRequestID(tc.name),
		)
		response := &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(
				strings.NewReader(
					`{"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=","size":"2x2"}],"usage":` + tc.usage + `}`,
				),
			),
		}
		result, providerErr := doubao.ImageHandler(m, c, response)
		require.Nil(t, providerErr)
		recordResult(
			c,
			m,
			price,
			&relaycontroller.HandleResult{Usage: result.Usage, UsageContext: result.UsageContext},
			0,
			true,
			nil,
		)

		var entry model.Log
		require.NoError(t, db.Where("request_id = ?", tc.name).First(&entry).Error)

		var outbox model.AsyncUsageInfo
		require.NoError(t, db.Where("log_id = ?", entry.ID).First(&outbox).Error)
		require.False(t, outbox.BalanceConsumeAttempted)
		require.False(t, outbox.BalanceConsumed)

		if tc.missing {
			require.Equal(t, model.AsyncUsageStatusMeasurementPending, outbox.Status)
			require.Equal(t, "incomplete", entry.UsageContext.ImageUsage.State)
			require.Equal(t, "pending", entry.Amount.ImageBillingResult.State)
			require.Zero(t, entry.Amount.UsedAmount)
		} else {
			require.Equal(t, "complete", entry.UsageContext.ImageUsage.State)
			require.Equal(t, int64(0), *entry.UsageContext.ImageUsage.InputCount)
			require.Equal(t, .3, entry.Amount.UsedAmount)
			require.Equal(t, model.AsyncUsageStatusPending, outbox.Status)
		}
	}
}
