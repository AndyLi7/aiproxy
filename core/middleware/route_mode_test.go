//nolint:testpackage
package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// Rejections before the distributor carry no relay mode. Since TokenAuth now
// attributes them to the key, a read route reached by POST must still log its
// read mode, or a disabled key on /v1/alpha/search would count as a call while
// every relay failure on that route is a read.
func TestRejectionsOnReadRoutesAreNotCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })

	token := model.TokenCache{ID: 61, Key: "route-mode-disabled", Name: "off", Group: "g", Status: model.TokenStatusDisabled}
	require.NoError(t, model.CacheSetToken(&token))
	t.Cleanup(func() { require.NoError(t, model.CacheDeleteToken(token.Key)) })

	handler := func(c *gin.Context) { t.Fatal("a disabled key must not reach the handler") }
	router := gin.New()
	router.Use(OperationalLogMiddleware())
	router.POST("/v1/alpha/search", TokenAuth, handler)
	router.POST("/v1/responses/:response_id/cancel", TokenAuth, handler)
	router.POST("/v1/chat/completions", TokenAuth, handler)
	router.POST("/v1/models/*model", TokenAuth, handler)

	for _, tc := range []struct {
		path string
		mode mode.Mode
		call bool
	}{
		{"/v1/alpha/search", mode.AlphaSearch, false},
		{"/v1/responses/resp_1/cancel", mode.ResponsesCancel, false},
		{"/v1/models/files-abc:download", mode.GeminiFiles, false},
		{"/v1/chat/completions", mode.Unknown, true},
		{"/v1/models/gemini-2.5-pro:generateContent", mode.Unknown, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			logs := captureOperationalLogs(t)
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+token.Key)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

			require.Len(t, *logs, 1)
			require.Equal(t, int(tc.mode), (*logs)[0].Mode)
			require.Equal(t, token.ID, (*logs)[0].TokenID)
			require.Equal(t, tc.call, (*logs)[0].IsCall())
		})
	}
}
