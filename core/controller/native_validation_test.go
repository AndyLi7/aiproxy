package controller_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/stretchr/testify/require"
)

func TestNativeContractValidationBoundedAndExact(t *testing.T) {
	router := gin.New()
	router.POST("/", controller.ValidateNativeContract)

	good := `{"version":1,"model":"test/model/native","input_schema":{"type":"object"},"output_schema":{"type":"null"}}`
	for _, tc := range []struct {
		body   string
		status int
	}{
		{good, 200},
		{good + "{}", 400},
		{strings.Replace(good, `"version":1`, `"version":1,"version":1`, 1), 400},
		{strings.Replace(good, `{"type":"null"}`, `{"$ref":"https://private.invalid/schema"}`, 1), 400},
		{strings.Repeat(" ", nativeresult.MaxBytes+1), 400},
	} {
		out := httptest.NewRecorder()
		router.ServeHTTP(
			out,
			httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/",
				strings.NewReader(tc.body),
			),
		)
		require.Equal(t, tc.status, out.Code, out.Body.String())

		if tc.status == 200 {
			require.Contains(
				t,
				out.Body.String(),
				fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(good))),
			)
			require.Contains(t, out.Body.String(), `"schemaCompiled":true`)
		}
	}
}

func TestNativeValidationRequiresAdmin(t *testing.T) {
	old := config.AdminKey
	config.AdminKey = "isolated-native-admin"
	t.Cleanup(func() { config.AdminKey = old })

	r := gin.New()
	r.POST("/", middleware.AdminAuth, controller.ValidateNativeContract)

	out := httptest.NewRecorder()
	r.ServeHTTP(
		out,
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{}`)),
	)
	require.Equal(t, 401, out.Code)
	require.NotContains(t, out.Body.String(), "schemaCompiled")
}
