package middleware

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivateImageRegistryRoutePreservesIdentity(t *testing.T) {
	const route = "tp-admin-demo-v1-hash::edit"
	config := map[model.ModelConfigKey]any{
		"public_model": "tp-admin-demo-v1-hash", "capability": "edit", "public_capability_model": "tp-admin-demo-v1-hash/edit",
		"x_token_platform_capability_contract": map[string]any{"entry_id": "vendor/image/edit", "contract": map[string]any{
			"entry_id": "vendor/image/edit", "validation_version": 1, "input_schema": map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"prompt"}, "additionalProperties": false},
		}},
	}
	for _, tc := range []struct {
		id, prompt string
		status     int
	}{{route, "cup", 0}, {route, "", 400}, {"other::edit", "cup", 503}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body, _ := json.Marshal(map[string]any{"model": tc.id, "prompt": tc.prompt})
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		err := validateImageRegistryRequest(c, mode.ImagesGenerations, tc.id, config)
		if tc.status != 0 {
			require.NotNil(t, err)
			require.Equal(t, tc.status, err.Status)
			continue
		}
		require.Nil(t, err)
		actual, readErr := common.GetRequestBodyReusable(c.Request)
		require.NoError(t, readErr)
		var forwarded map[string]any
		require.NoError(t, json.Unmarshal(actual, &forwarded))
		require.Equal(t, route, forwarded["model"])
	}
}

func TestImageRegistryGuardStopsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		missing       bool
		status, calls int
	}{
		{"invalid", `{"model":"vendor/image/text-to-image","prompt":""}`, false, 400, 0},
		{"valid", `{"model":"vendor/image/text-to-image","prompt":"cup"}`, false, 204, 1},
		{"missing contract", `{"model":"vendor/image/text-to-image","prompt":"cup"}`, true, 503, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[model.ModelConfigKey]any{"public_capability_model": "vendor/image/text-to-image", "capability_contract_version": 1}
			if !tc.missing {
				config["x_token_platform_capability_contract"] = map[string]any{"entry_id": "vendor/image/text-to-image", "contract": map[string]any{"entry_id": "vendor/image/text-to-image", "validation_version": 1, "input_schema": map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"prompt"}, "additionalProperties": false}}}
			}
			calls := 0
			router := gin.New()
			router.POST("/", func(c *gin.Context) {
				if err := validateImageRegistryRequest(c, mode.ImagesGenerations, "vendor/image/text-to-image", config); err != nil {
					c.AbortWithStatus(err.Status)
					return
				}
				c.Next()
			}, func(c *gin.Context) { calls++; c.Status(204) })
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, tc.status, recorder.Code)
			require.Equal(t, tc.calls, calls)
		})
	}
}
