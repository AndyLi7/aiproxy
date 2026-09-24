package middleware

import (
	"context"
	"encoding/json"
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

func TestPrivateImageRegistryRoutePreservesIdentity(t *testing.T) {
	const route = "tp-admin-demo-v1-hash::edit"

	config := map[model.ModelConfigKey]any{
		"public_model":            "tp-admin-demo-v1-hash",
		"capability":              "edit",
		"public_capability_model": "tp-admin-demo-v1-hash/edit",
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": "vendor/image/edit",
			"contract": map[string]any{
				"entry_id":           "vendor/image/edit",
				"validation_version": 1,
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{"type": "string", "minLength": 1},
					},
					"required":             []string{"prompt"},
					"additionalProperties": false,
				},
			},
		},
	}
	for _, tc := range []struct {
		id, prompt string
		status     int
	}{{route, "cup", 0}, {route, "", 400}, {"other::edit", "cup", 503}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body := mustMarshalJSON(t, map[string]any{"model": tc.id, "prompt": tc.prompt})
		c.Request = httptest.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			"/",
			strings.NewReader(string(body)),
		)
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

func TestPrivateImageRegistryCustomCapabilityPreservesIdentity(t *testing.T) {
	const route = "tp-admin-demo-v1-hash::subject-reference"

	config := map[model.ModelConfigKey]any{
		"public_model":            "tp-admin-demo-v1-hash",
		"capability":              "subject-reference",
		"public_capability_model": "tp-admin-demo-v1-hash/subject-reference",
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": "vendor/image/subject-reference",
			"contract": map[string]any{
				"entry_id":           "vendor/image/subject-reference",
				"validation_version": 1,
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{"type": "string", "minLength": 1},
					},
					"required":             []string{"prompt"},
					"additionalProperties": false,
				},
			},
		},
	}
	for _, tc := range []struct {
		id, prompt string
		status     int
	}{{route, "cup", 0}, {route, "", 400}, {"other::subject-reference", "cup", 503}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body := mustMarshalJSON(t, map[string]any{"model": tc.id, "prompt": tc.prompt})
		c.Request = httptest.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			"/",
			strings.NewReader(string(body)),
		)
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
			config := map[model.ModelConfigKey]any{
				"public_capability_model":     "vendor/image/text-to-image",
				"capability_contract_version": 1,
			}
			if !tc.missing {
				config["x_token_platform_capability_contract"] = map[string]any{
					"entry_id": "vendor/image/text-to-image",
					"contract": map[string]any{
						"entry_id":           "vendor/image/text-to-image",
						"validation_version": 1,
						"input_schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"prompt": map[string]any{"type": "string", "minLength": 1},
							},
							"required":             []string{"prompt"},
							"additionalProperties": false,
						},
					},
				}
			}

			calls := 0
			router := gin.New()
			router.POST("/", func(c *gin.Context) {
				if err := validateImageRegistryRequest(
					c,
					mode.ImagesGenerations,
					"vendor/image/text-to-image",
					config,
				); err != nil {
					c.AbortWithStatus(err.Status)
					return
				}

				c.Next()
			}, func(c *gin.Context) { calls++; c.Status(204) })

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				"/",
				strings.NewReader(tc.body),
			)
			req.Header.Set("Content-Type", "application/json")

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, tc.status, recorder.Code)
			require.Equal(t, tc.calls, calls)
		})
	}
}

func TestResolvedBaseImageRegistryPreservesRequestedIdentity(t *testing.T) {
	const (
		parent       = "vendor/image"
		capabilityID = parent + "/text-to-image"
	)

	config := map[model.ModelConfigKey]any{
		"public_model":            parent,
		"capability":              "text-to-image",
		"public_capability_model": capabilityID,
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": capabilityID,
			"contract": map[string]any{
				"entry_id":           capabilityID,
				"validation_version": 1,
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{"type": "string"},
					},
					"required":             []string{"prompt"},
					"additionalProperties": false,
				},
			},
		},
	}
	for _, requested := range []string{parent, capabilityID} {
		t.Run(requested, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			body := mustMarshalJSON(t, map[string]any{"model": requested, "prompt": "A teapot"})
			c.Request = httptest.NewRequestWithContext(context.Background(),
				http.MethodPost,
				"/v1/images/generations",
				strings.NewReader(string(body)),
			)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(RequestedModel, requested)
			c.Set(PublicModel, parent)
			c.Set(PublicCapabilityModel, capabilityID)
			c.Set(ResolvedCapability, "text-to-image")
			require.Nil(
				t,
				validateImageRegistryRequest(c, mode.ImagesGenerations, requested, config),
			)
			raw, err := common.GetRequestBodyReusable(c.Request)
			require.NoError(t, err)

			var result map[string]any
			require.NoError(t, json.Unmarshal(raw, &result))
			require.Equal(t, requested, result["model"])
		})
	}
}

func TestAsyncImageRegistryRejectsSyncEndpointBeforeSize(t *testing.T) {
	const publicID = "alibaba/2.2-5b/text-to-image"
	config := map[model.ModelConfigKey]any{
		"public_capability_model": publicID,
		"x_token_platform_capability_contract": map[string]any{
			"entry_id": publicID,
			"contract": map[string]any{
				"entry_id":           publicID,
				"validation_version": 1,
				"execution":          map[string]any{"mode": "async"},
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{"type": "string", "minLength": 1},
						"image_size": map[string]any{"type": "object", "properties": map[string]any{
							"width":  map[string]any{"type": "integer"},
							"height": map[string]any{"type": "integer"},
						}},
					},
					"required":             []string{"prompt"},
					"additionalProperties": false,
				},
			},
		},
	}
	for _, requestMode := range []mode.Mode{mode.ImagesGenerations, mode.ImagesEdits} {
		for _, tc := range []struct {
			path     string
			body     string
			wantCode string
		}{
			{"/v1/images/generations", `{"model":"alibaba/2.2-5b/text-to-image","prompt":"lion","size":"1536x1024"}`, "unsupported_endpoint"},
			{"/v1/images/tasks", `{"model":"alibaba/2.2-5b/text-to-image","prompt":"lion","size":"1536x1024"}`, ""},
			{"/v1/images/tasks", `{"model":"alibaba/2.2-5b/text-to-image","prompt":"lion","image_size":{"width":1536,"height":1024}}`, ""},
		} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			err := validateImageRegistryRequest(c, requestMode, publicID, config)
			if tc.wantCode != "" {
				require.NotNil(t, err)
				require.Equal(t, tc.wantCode, err.Code)
				require.Equal(t, "endpoint", err.Param)
				continue
			}
			require.Nil(t, err)
			forwarded, readErr := common.GetRequestBodyReusable(c.Request)
			require.NoError(t, readErr)
			var values map[string]any
			require.NoError(t, json.Unmarshal(forwarded, &values))
			require.NotContains(t, values, "size", "normalized request must not retain the consumed alias")
			require.Equal(t, map[string]any{"width": float64(1536), "height": float64(1024)}, values["image_size"])
		}
	}
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)

	return data
}
