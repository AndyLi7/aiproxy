//nolint:testpackage // White-box coverage needs the package-private routingHTTPFixture fixture or implementation.
package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
)

func TestAccessConfigurationHTTPAuthAbsentReplayAndSecretSafety(t *testing.T) {
	routingHTTPFixture(t)
	require.NoError(t, model.DB.AutoMigrate(&model.ChannelAccessConfiguration{}))

	r := gin.New()
	api := r.Group(
		"/api/access-configurations",
		RequireMigrationInventoryHeaderAuth,
		middleware.AdminAuth,
	)
	api.GET("/:id", GetAccessConfiguration)
	api.PUT("/:id", EnsureAccessConfiguration)

	s := httptest.NewServer(r)
	defer s.Close()

	call := func(method, auth string, body []byte) (int, map[string]any, string) {
		req, e := http.NewRequestWithContext(t.Context(),
			method,
			s.URL+"/api/access-configurations/test-access",
			bytes.NewReader(body),
		)
		require.NoError(t, e)

		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}

		response, e := s.Client().Do(req)
		require.NoError(t, e)

		defer response.Body.Close()

		raw, e := io.ReadAll(response.Body)
		require.NoError(t, e)

		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope))

		return response.StatusCode, envelope, string(raw)
	}
	status, _, _ := call("GET", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	status, absent, _ := call("GET", "synthetic-routing-admin", nil)
	require.Equal(t, 200, status)
	require.Contains(t, absent, "data")
	require.Nil(t, absent["data"])

	body := []byte(
		`{"name":"test access","key":"synthetic-provider-access-key","type":1,"baseUrl":"https://example.invalid"}`,
	)
	status, first, raw := call("PUT", "synthetic-routing-admin", body)
	require.Equal(t, 200, status)
	require.NotContains(t, raw, "synthetic-provider-access-key")

	status, replay, _ := call("PUT", "synthetic-routing-admin", body)
	require.Equal(t, 200, status)
	require.Equal(t, first, replay)

	status, _, raw = call(
		"PUT",
		"synthetic-routing-admin",
		bytes.ReplaceAll(
			body,
			[]byte("synthetic-provider-access-key"),
			[]byte("different-provider-access-key"),
		),
	)
	require.Equal(t, 409, status)
	require.NotContains(t, raw, "different-provider-access-key")
}
