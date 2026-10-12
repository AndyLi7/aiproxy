//nolint:testpackage
package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

// D4 (2026-10-12): the source is derived from the authenticated credential
// through the real TokenAuth. The website Playground's hidden credential is
// playground; a playground header from any other key is api. Rejection rows
// carry a billing operation only once a handler set one.
func TestUsageSourceThroughTokenAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldDB, oldRedis := model.DB, common.RedisEnabled
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Group{}, &model.GroupModelConfig{}, &model.Token{}))
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, common.RedisEnabled = oldDB, oldRedis })

	for _, group := range []string{"u_source", "u_source_disabled"} {
		require.NoError(t, db.Create(&model.Group{ID: group, Status: model.GroupStatusEnabled}).Error)
	}

	create := func(group, name string, status int) model.Token {
		token := model.Token{GroupID: group, Name: model.EmptyNullString(name), Status: status}
		require.NoError(t, db.Create(&token).Error)

		return token
	}
	hidden := create("u_source", model.PlaygroundCredentialName, model.TokenStatusEnabled)
	visible := create("u_source", "playground", model.TokenStatusEnabled)
	disabledHidden := create("u_source_disabled", model.PlaygroundCredentialName, model.TokenStatusDisabled)

	for _, tc := range []struct {
		name       string
		key        string
		header     string
		operation  string
		wantStatus int
		wantSource string
	}{
		{name: "hidden credential", key: hidden.Key, wantStatus: http.StatusPaymentRequired, wantSource: model.RequestSourcePlayground},
		// Old: playground, from the unsigned header.
		{name: "visible key named playground with the header", key: visible.Key, header: model.RequestSourcePlayground, wantStatus: http.StatusPaymentRequired, wantSource: model.RequestSourceAPI},
		{name: "hidden credential with an admin_demo header", key: hidden.Key, header: model.RequestSourceAdminDemo, wantStatus: http.StatusPaymentRequired, wantSource: model.RequestSourcePlayground},
		{name: "rejected hidden credential", key: disabledHidden.Key, wantStatus: http.StatusForbidden, wantSource: model.RequestSourcePlayground},
		{name: "rejection after a hold", key: hidden.Key, operation: "native:held", wantStatus: http.StatusServiceUnavailable, wantSource: model.RequestSourcePlayground},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureOperationalLogs(t)

			router := gin.New()
			router.Use(OperationalLogMiddleware())
			router.POST("/v1/model-tasks", TokenAuth, func(c *gin.Context) {
				if tc.operation != "" {
					SetCallBillingOperationID(c, tc.operation)
					SetOperationalFailure(c, model.FailureStageRouting, "native_execution_unavailable", "unavailable")
					c.Status(http.StatusServiceUnavailable)

					return
				}

				SetFailureStage(c, model.FailureStageBalance, "insufficient balance")
				c.Status(http.StatusPaymentRequired)
			})

			request := httptest.NewRequest(http.MethodPost, "/v1/model-tasks", strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+tc.key)
			if tc.header != "" {
				request.Header.Set(OperationalLogSourceHeader, tc.header)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)

			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			require.Len(t, *logs, 1)

			row := (*logs)[0]
			require.Equal(t, tc.wantSource, row.RequestSource)
			require.Equal(t, model.EmptyNullString(tc.operation), row.BillingOperationID)

			if tc.operation != "" {
				require.Equal(t, model.CallStatusFailed, row.CallStatus(row.RequestAt))
			} else {
				require.Equal(t, model.CallStatusRejected, row.CallStatus(row.RequestAt))
			}
		})
	}
}
