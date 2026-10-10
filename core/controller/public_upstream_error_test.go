//nolint:testpackage
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/model"
	relaycontroller "github.com/labring/aiproxy/core/relay/controller"
	"github.com/labring/aiproxy/core/relay/meta"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Upstream 401/402/403/404 concern the provider account or route. Passing them
// through made customers believe their own key, balance or model was wrong.
func TestUpstreamAccountFailuresAreNotReportedAsCustomerErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const providerFailure = "The upstream provider could not process this request. Retry later or contact support with the request ID."
	for _, tc := range []struct {
		upstream, status int
		code, message    string
	}{
		{401, 502, "upstream_unavailable", providerFailure},
		{402, 502, "upstream_unavailable", providerFailure},
		{403, 502, "upstream_unavailable", providerFailure},
		{404, 502, "upstream_unavailable", providerFailure},
		{400, 400, "invalid_request", "The request could not be processed. Please check the input parameters."},
		{422, 422, "invalid_request", "The request could not be processed. Please check the input parameters."},
		{429, 429, "upstream_unavailable", "The service is temporarily unavailable. Please try again later."},
		{503, 503, "upstream_unavailable", "The service is temporarily unavailable. Please try again later."},
	} {
		t.Run(strconv.Itoa(tc.upstream), func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			writePublicUpstreamError(c, relaymodel.WrapperOpenAIError(
				errors.New("provider key invalid"), "provider_code", tc.upstream))

			require.Equal(t, tc.status, w.Code)
			var body struct {
				Error struct{ Code, Message, Type string } `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tc.code, body.Error.Code)
			require.Equal(t, tc.message, body.Error.Message)
			require.NotContains(t, w.Body.String(), "provider key invalid")
		})
	}
}

func TestUpstreamFailureLogRecordsReturnedStatusAndCode(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AsyncUsageInfo{}))
	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/generations", nil)
	price := model.Price{
		OutputPrice:     1,
		OutputPriceUnit: 1,
		ImageBilling:    &model.ImageBillingPolicy{Version: 1, Scenario: "generation"},
	}

	for _, tc := range []struct {
		upstream, status int
		code             string
	}{{403, 502, "upstream_unavailable"}, {400, 400, "invalid_request"}} {
		m := &meta.Meta{RequestID: "upstream-" + strconv.Itoa(tc.upstream), OriginModel: "image"}
		result := &relaycontroller.HandleResult{
			Error: relaymodel.WrapperOpenAIError(errors.New("provider says no"), "provider_code", tc.upstream),
		}
		recordResult(c, m, price, result, 0, true, nil)

		require.Equal(t, tc.code, m.OperationalFields.ErrorCode)
		require.Equal(t, model.FailureStageUpstream, m.OperationalFields.FailureStage)
		var entry model.Log
		require.NoError(t, db.Where("request_id = ?", m.RequestID).First(&entry).Error)
		require.Equal(t, tc.status, entry.Code)
		require.Equal(t, tc.code, entry.ErrorCode)
		// Operators keep the provider's original status in the audit text.
		require.Contains(t, entry.SafeError, "status code: "+strconv.Itoa(tc.upstream))
	}
}
